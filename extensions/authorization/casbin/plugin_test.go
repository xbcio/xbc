package casbin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/xbcio/xbc/extensions/authorization/rbac"
	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

func TestDefinitionIsCanonicalAndBundleIsStable(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero {
		t.Fatal("Definition() returned a zero handle")
	}
	if Definition() != Definition() {
		t.Fatal("Definition() returned different handles")
	}
	first, second := Bundle(), Bundle()
	if reflect.DeepEqual(first, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("Bundle() returned different composition content")
	}
}

// TestDefinitionExportsExactlyEnforcerAndBackendContracts pins the split's
// boundary from the engine side: the engine exports the two
// protocol-neutral contracts and nothing else. Web route enforcement is a
// separate product, so a web.Middleware export here -- or any third contract
// -- would be a boundary regression this test exists to catch.
func TestDefinitionExportsExactlyEnforcerAndBackendContracts(t *testing.T) {
	descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(Definition()))
	if !ok {
		t.Fatal("Definition() returned a zero handle")
	}

	want := map[reflect.Type]bool{
		reflect.TypeOf((*EnforcerProvider)(nil)).Elem(): false,
		reflect.TypeOf((*rbac.Backend)(nil)).Elem():     false,
	}
	if len(descriptor.Contracts) != len(want) {
		t.Fatalf("Definition declares %d contracts, want exactly %d: %+v", len(descriptor.Contracts), len(want), descriptor.Contracts)
	}
	for _, contract := range descriptor.Contracts {
		if _, expected := want[contract.Type]; !expected {
			t.Errorf("Definition exports unexpected contract %v", contract.Type)
			continue
		}
		want[contract.Type] = true
	}
	for contract, found := range want {
		if !found {
			t.Errorf("Definition contracts %+v do not export %v", descriptor.Contracts, contract)
		}
	}
}

func TestNewBuildsEnforcerImmediatelyAndStopDeactivatesIt(t *testing.T) {
	p, err := New(DefaultConfig())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	enforcer, ok := p.Enforcer()
	if !ok || enforcer == nil {
		t.Fatal("New() did not build an active enforcer")
	}

	host := newTestHost()
	if err := p.start(testContext(host)); err != nil {
		t.Fatalf("start() error = %v", err)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	host.close()
	if _, ok := p.Enforcer(); ok {
		t.Fatal("Enforcer remained active after Stop")
	}
}

// TestRequestConventionReportsTheValidatedConvention pins the EnforcerProvider
// half that lets a consumer build Enforce's request tuple without reading the
// enforcer's model, which LoadPolicy is free to replace underneath it. The
// value must be the convention the model was built and validated against, and
// it must stay readable after Stop, when Enforcer itself is no longer handed
// out.
func TestRequestConventionReportsTheValidatedConvention(t *testing.T) {
	for _, convention := range []RequestConvention{ConventionRoutePermission, ConventionPathMethod} {
		t.Run(string(convention), func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.RequestConvention = convention
			p, err := New(cfg)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if got := p.RequestConvention(); got != convention {
				t.Fatalf("RequestConvention() = %q, want %q", got, convention)
			}

			host := newTestHost()
			if err := p.start(testContext(host)); err != nil {
				t.Fatalf("start() error = %v", err)
			}
			if err := p.Stop(context.Background()); err != nil {
				t.Fatalf("Stop() error = %v", err)
			}
			host.close()
			if got := p.RequestConvention(); got != convention {
				t.Fatalf("RequestConvention() after Stop = %q, want %q", got, convention)
			}
		})
	}
}

// TestRequestConventionValidatesEveryModelSource pins the agreement the
// EnforcerProvider contract promises across all three model sources the engine
// builds from: a model whose request definition matches the configured
// convention is built and reported, and a mismatch is refused during
// construction. Without the refusal half, a configured convention and the
// enforcer it runs against could disagree -- a consumer would build the wrong
// request tuple while RequestConvention reports the configured value.
func TestRequestConventionValidatesEveryModelSource(t *testing.T) {
	threeFieldModel := `[request_definition]
r = sub, obj, act

[policy_definition]
p = sub, obj, act

[role_definition]
g = _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = (r.sub == p.sub || g(r.sub, p.sub)) && keyMatch2(r.obj, p.obj) && regexMatch(r.act, p.act)
`
	modelFile := filepath.Join(t.TempDir(), "model.conf")
	if err := os.WriteFile(modelFile, []byte(threeFieldModel), 0o600); err != nil {
		t.Fatalf("write model file: %v", err)
	}

	tests := []struct {
		name       string
		mutate     func(*Config)
		convention RequestConvention
		wantErr    bool
	}{
		{name: "built-in default", convention: ConventionRoutePermission},
		{name: "built-in path_method", convention: ConventionPathMethod},
		{name: "inline model matches", mutate: func(c *Config) { c.Model = threeFieldModel }, convention: ConventionPathMethod},
		{name: "inline model mismatches", mutate: func(c *Config) { c.Model = threeFieldModel }, convention: ConventionRoutePermission, wantErr: true},
		{name: "model file matches", mutate: func(c *Config) { c.ModelFile = modelFile }, convention: ConventionPathMethod},
		{name: "model file mismatches", mutate: func(c *Config) { c.ModelFile = modelFile }, convention: ConventionRoutePermission, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.RequestConvention = test.convention
			if test.mutate != nil {
				test.mutate(&cfg)
			}
			p, err := New(cfg)
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "request_convention") {
					t.Fatalf("New() error = %v, want a request_convention shape refusal", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if got := p.RequestConvention(); got != test.convention {
				t.Fatalf("RequestConvention() = %q, want %q", got, test.convention)
			}
			enforcer, ok := p.Enforcer()
			if !ok {
				t.Fatal("New() did not build an active enforcer")
			}
			request, ok := enforcer.GetModel()["r"]["r"]
			if !ok || request == nil {
				t.Fatal("built model declares no request definition r")
			}
			want := 2
			if test.convention == ConventionPathMethod {
				want = 3
			}
			if len(request.Tokens) != want {
				t.Fatalf("model request r has %d fields, convention %q requires %d", len(request.Tokens), test.convention, want)
			}
		})
	}
}

func TestNewRejectsInvalidModelFile(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ModelFile = "missing-model.conf"
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "model_file") {
		t.Fatalf("New() error = %v, want model_file failure", err)
	}

	cfg.ModelFile = ""
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() with corrected config error = %v", err)
	}
	if _, ok := p.Enforcer(); !ok {
		t.Fatal("New() with corrected config did not build an active enforcer")
	}
}

func TestLifecycleOperationErrorsAndIdempotentStop(t *testing.T) {
	p, err := New(DefaultConfig())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := p.start(nil); err == nil {
		t.Fatal("start(nil) error = nil")
	}

	host := newTestHost()
	if err := p.start(testContext(host)); err != nil {
		t.Fatal(err)
	}
	if err := p.start(testContext(host)); err == nil {
		t.Fatal("second start() error = nil")
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
	if !errors.Is(p.LoadPolicy(), ErrStopped) {
		t.Fatalf("LoadPolicy() after Stop = %v, want ErrStopped", p.LoadPolicy())
	}
	if err := p.start(testContext(host)); !errors.Is(err, ErrStopped) {
		t.Fatalf("start() after Stop = %v, want ErrStopped", err)
	}
	host.close()
}
