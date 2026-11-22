package casbinx

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/casbin/casbin/v2/util"

	"github.com/xbcio/xbc/extensions/authorization/rbac"
)

const noRoleDefinitionModel = `[request_definition]
r = sub, perm

[policy_definition]
p = sub, perm

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = r.sub == p.sub && r.perm == p.perm
`

// TestWithAdapterLoadsExternalAdapterDuringConstruction pins the timing
// difference between direct construction and the configured-provider path:
// New attaches and loads an injected adapter before it returns, so Enforce is
// answerable without Start, and autosave is on unless the caller opts out.
func TestWithAdapterLoadsExternalAdapterDuringConstruction(t *testing.T) {
	adapter := &testAdapter{}
	adapter.setRules([]string{"p", "alice", "reports:read"})

	p, err := New(DefaultConfig(), WithAdapter(adapter))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	enforcer, ok := p.Enforcer()
	if !ok {
		t.Fatal("New() did not build an active enforcer")
	}
	if got := enforcer.GetAdapter(); got != adapter {
		t.Fatalf("enforcer adapter = %#v, want the injected adapter", got)
	}
	if loads, _ := adapter.counts(); loads != 1 {
		t.Fatalf("adapter LoadPolicy calls during construction = %d, want 1", loads)
	}
	allowed, err := enforcer.Enforce("alice", "reports:read")
	assertEnforce(t, allowed, err, true)

	added, err := enforcer.AddPolicy("bob", "reports:read")
	if err != nil || !added {
		t.Fatalf("AddPolicy() = (%v, %v), want (true, nil)", added, err)
	}
	if _, adds := adapter.counts(); adds != 1 {
		t.Fatalf("adapter AddPolicy calls = %d, want autosave to be on by default", adds)
	}
}

// TestWithAutoSaveControlsExternalAdapterWrites pins the autosave switch: with
// autosave on (the default, and the explicit true case) a mutation reaches the
// adapter and survives LoadPolicy; with it off the mutation stays in memory
// and LoadPolicy restores the adapter's contents.
func TestWithAutoSaveControlsExternalAdapterWrites(t *testing.T) {
	tests := []struct {
		name       string
		options    []Option
		wantAdds   int
		wantReload bool
	}{
		{name: "default", wantAdds: 1, wantReload: true},
		{name: "explicit enabled", options: []Option{WithAutoSave(true)}, wantAdds: 1, wantReload: true},
		{name: "disabled", options: []Option{WithAutoSave(false)}, wantAdds: 0, wantReload: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter := &testAdapter{}
			adapter.setRules([]string{"p", "alice", "reports:read"})
			options := append([]Option{WithAdapter(adapter)}, test.options...)
			p, err := New(DefaultConfig(), options...)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			enforcer, _ := p.Enforcer()

			added, err := enforcer.AddPolicy("bob", "reports:read")
			if err != nil || !added {
				t.Fatalf("AddPolicy() = (%v, %v), want in-memory addition", added, err)
			}
			if _, adds := adapter.counts(); adds != test.wantAdds {
				t.Fatalf("adapter AddPolicy calls = %d, want %d", adds, test.wantAdds)
			}
			allowed, err := enforcer.Enforce("bob", "reports:read")
			assertEnforce(t, allowed, err, true)

			if err := p.LoadPolicy(); err != nil {
				t.Fatalf("LoadPolicy() error = %v", err)
			}
			allowed, err = enforcer.Enforce("bob", "reports:read")
			assertEnforce(t, allowed, err, test.wantReload)
		})
	}
}

// TestWithRoleMatchingFuncGovernsRoleLinksAfterLoad pins the registration
// point: the function is in force for the role links built by the initial
// policy load and stays in force across LoadPolicy. The same policy is denied
// without the option and allowed with it, for both policy sources the direct
// constructor supports.
func TestWithRoleMatchingFuncGovernsRoleLinksAfterLoad(t *testing.T) {
	const policy = "p, reader:*, /reports, GET\ng, alice, reader:1\n"
	sources := []struct {
		name  string
		build func(t *testing.T, extra ...Option) *Plugin
	}{
		{
			name: "inline policy",
			build: func(t *testing.T, extra ...Option) *Plugin {
				t.Helper()
				cfg := DefaultConfig()
				cfg.RequestConvention = ConventionPathMethod
				cfg.Policy = policy
				p, err := New(cfg, extra...)
				if err != nil {
					t.Fatalf("New() error = %v", err)
				}
				return p
			},
		},
		{
			name: "injected adapter",
			build: func(t *testing.T, extra ...Option) *Plugin {
				t.Helper()
				adapter := &testAdapter{}
				adapter.setRules(
					[]string{"p", "reader:*", "/reports", "GET"},
					[]string{"g", "alice", "reader:1"},
				)
				cfg := DefaultConfig()
				cfg.RequestConvention = ConventionPathMethod
				p, err := New(cfg, append([]Option{WithAdapter(adapter)}, extra...)...)
				if err != nil {
					t.Fatalf("New() error = %v", err)
				}
				return p
			},
		},
	}

	for _, source := range sources {
		t.Run(source.name, func(t *testing.T) {
			plain := source.build(t)
			enforcer, _ := plain.Enforcer()
			allowed, err := enforcer.Enforce("alice", "/reports", "GET")
			assertEnforce(t, allowed, err, false)

			matched := source.build(t, WithRoleMatchingFunc("g", "KeyMatch2", util.KeyMatch2))
			enforcer, _ = matched.Enforcer()
			allowed, err = enforcer.Enforce("alice", "/reports", "GET")
			assertEnforce(t, allowed, err, true)

			if err := matched.LoadPolicy(); err != nil {
				t.Fatalf("LoadPolicy() error = %v", err)
			}
			allowed, err = enforcer.Enforce("alice", "/reports", "GET")
			assertEnforce(t, allowed, err, true)
		})
	}
}

// TestWithRoleMatchingFuncValidation pins the fail-closed registration checks:
// a malformed option or a model without the named role definition is refused
// instead of silently registering nothing.
func TestWithRoleMatchingFuncValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		option  Option
		wantErr string
	}{
		{
			name:    "nil function",
			option:  WithRoleMatchingFunc("g", "KeyMatch2", nil),
			wantErr: "non-nil function",
		},
		{
			name:    "empty ptype",
			option:  WithRoleMatchingFunc(" ", "KeyMatch2", util.KeyMatch2),
			wantErr: "non-empty ptype",
		},
		{
			name:    "empty name",
			option:  WithRoleMatchingFunc("g", " ", util.KeyMatch2),
			wantErr: "non-empty name",
		},
		{
			name:    "unknown ptype",
			option:  WithRoleMatchingFunc("g2", "KeyMatch2", util.KeyMatch2),
			wantErr: `no role definition "g2"`,
		},
		{
			name:    "model without role definition",
			cfg:     Config{Model: noRoleDefinitionModel},
			option:  WithRoleMatchingFunc("g", "KeyMatch2", util.KeyMatch2),
			wantErr: "no role definition",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := test.cfg
			if cfg.Model == "" {
				cfg = DefaultConfig()
			}
			_, err := New(cfg, test.option)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("New() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

// TestAdapterAndOptionConflictsAreRejected pins source selection: an injected
// adapter is the policy source, so it conflicts with inline/file policy and
// with a configured adapter provider, and autosave has no meaning without one.
func TestAdapterAndOptionConflictsAreRejected(t *testing.T) {
	adapter := &testAdapter{}
	tests := []struct {
		name    string
		cfg     func() Config
		options []Option
		wantErr string
	}{
		{
			name:    "nil adapter",
			cfg:     DefaultConfig,
			options: []Option{WithAdapter(nil)},
			wantErr: "non-nil adapter",
		},
		{
			name:    "typed nil adapter",
			cfg:     DefaultConfig,
			options: []Option{WithAdapter((*testAdapter)(nil))},
			wantErr: "non-nil adapter",
		},
		{
			name: "inline policy",
			cfg: func() Config {
				cfg := DefaultConfig()
				cfg.Policy = "p, alice, reports:read"
				return cfg
			},
			options: []Option{WithAdapter(adapter)},
			wantErr: "adapter is mutually exclusive",
		},
		{
			name: "policy file",
			cfg: func() Config {
				cfg := DefaultConfig()
				cfg.PolicyFile = "policy.csv"
				return cfg
			},
			options: []Option{WithAdapter(adapter)},
			wantErr: "adapter is mutually exclusive",
		},
		{
			name: "configured adapter provider",
			cfg: func() Config {
				cfg := DefaultConfig()
				cfg.Adapter = ProviderRef{Plugin: "casbin-gorm"}
				return cfg
			},
			options: []Option{WithAdapter(adapter)},
			wantErr: "mutually exclusive with the configured adapter provider",
		},
		{
			name:    "autosave without adapter",
			cfg:     DefaultConfig,
			options: []Option{WithAutoSave(false)},
			wantErr: "requires WithAdapter",
		},
		{
			name: "configured watcher with injected adapter",
			cfg: func() Config {
				cfg := DefaultConfig()
				cfg.Watcher = ProviderRef{Plugin: "casbin-redis"}
				return cfg
			},
			options: []Option{WithAdapter(adapter)},
			wantErr: "watcher requires an adapter provider",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(test.cfg(), test.options...)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("New() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

// TestInjectedAdapterSupportsReloadIntervalAndStart pins that an injected
// adapter satisfies reload_interval's source requirement, that Start does not
// load it a second time (New already did), and that the managed reload task
// still refreshes it.
func TestInjectedAdapterSupportsReloadIntervalAndStart(t *testing.T) {
	adapter := &testAdapter{}
	adapter.setRules([]string{"p", "alice", "reports:read"})
	cfg := DefaultConfig()
	cfg.ReloadInterval = 5 * time.Millisecond
	p, err := New(cfg, WithAdapter(adapter))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	host := newTestHost()
	if err := p.start(testContext(host)); err != nil {
		host.close()
		t.Fatalf("start() error = %v", err)
	}
	t.Cleanup(func() {
		_ = p.Stop(context.Background())
		host.close()
	})
	if loads, _ := adapter.counts(); loads != 1 {
		t.Fatalf("adapter LoadPolicy calls after Start = %d, want only the construction load", loads)
	}

	adapter.setRules([]string{"p", "bob", "reports:read"})
	enforcer, _ := p.Enforcer()
	waitFor(t, time.Second, func() bool {
		allowed, err := enforcer.Enforce("bob", "reports:read")
		return err == nil && allowed
	}, "periodic reload did not load the injected adapter")
}

// TestInjectedAdapterCapabilitiesFollowModelAndAdapter pins the interaction
// with request_convention: the model shape decides whether the rbac.Backend
// accepts mutations (3/3/3), and the adapter's interfaces decide whether they
// are offered, regardless of whether the adapter came from a provider or from
// WithAdapter. A model that disagrees with the convention is refused before
// the adapter is read at all.
func TestInjectedAdapterCapabilitiesFollowModelAndAdapter(t *testing.T) {
	t.Run("path_method exposes mutation through injected batch adapter", func(t *testing.T) {
		adapter := newBackendTestAdapter()
		cfg := DefaultConfig()
		cfg.RequestConvention = ConventionPathMethod
		p, err := New(cfg, WithAdapter(adapter))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		want := rbac.Capabilities{
			RequestFields:         3,
			PolicyFields:          3,
			GroupingFields:        2,
			Mutable:               true,
			FilteredPolicyReplace: true,
		}
		if got := p.Capabilities(); got != want {
			t.Fatalf("Capabilities() = %+v, want %+v", got, want)
		}

		changed, err := p.ReplaceRolePermissions(context.Background(), "admin", []rbac.Permission{{Object: "/reports", Action: "GET"}})
		if err != nil || !changed {
			t.Fatalf("ReplaceRolePermissions() = (%v, %v), want (true, nil)", changed, err)
		}
		enforcer, _ := p.Enforcer()
		allowed, err := enforcer.Enforce("admin", "/reports", "GET")
		assertEnforce(t, allowed, err, true)
	})

	t.Run("route_permission refuses mutation", func(t *testing.T) {
		adapter := newBackendTestAdapter()
		p, err := New(DefaultConfig(), WithAdapter(adapter))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if got, want := p.Capabilities(), (rbac.Capabilities{RequestFields: 2, PolicyFields: 2, GroupingFields: 2, Mutable: true, FilteredPolicyReplace: true}); got != want {
			t.Fatalf("Capabilities() = %+v, want %+v", got, want)
		}
		_, err = p.ReplaceRolePermissions(context.Background(), "admin", []rbac.Permission{{Object: "reports:read"}})
		if err == nil || !strings.Contains(err.Error(), "requires r/p/g field counts") {
			t.Fatalf("ReplaceRolePermissions() error = %v, want a request-shape refusal", err)
		}
	})

	t.Run("convention mismatch is refused before the adapter is read", func(t *testing.T) {
		adapter := &testAdapter{}
		cfg := DefaultConfig()
		cfg.Model = pathMethodModel
		_, err := New(cfg, WithAdapter(adapter))
		if err == nil || !strings.Contains(err.Error(), "request_convention") {
			t.Fatalf("New() error = %v, want a request_convention shape refusal", err)
		}
		if loads, _ := adapter.counts(); loads != 0 {
			t.Fatalf("adapter LoadPolicy calls after a refused construction = %d, want 0", loads)
		}
	})
}
