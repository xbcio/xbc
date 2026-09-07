package casbin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	casbinlib "github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"
	"github.com/casbin/casbin/v2/persist"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

type testAdapterProvider struct{ adapter persist.Adapter }

func (p *testAdapterProvider) Adapter() persist.Adapter {
	if p == nil {
		return nil
	}
	return p.adapter
}

type testAdapter struct {
	mu sync.Mutex

	rules       [][]string
	loadErr     error
	loadCalls   int
	addCalls    int
	removeCalls int
}

var _ persist.Adapter = (*testAdapter)(nil)

func (a *testAdapter) LoadPolicy(target model.Model) error {
	a.mu.Lock()
	a.loadCalls++
	err := a.loadErr
	rules := cloneRules(a.rules)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	for _, rule := range rules {
		if err := persist.LoadPolicyArray(rule, target); err != nil {
			return err
		}
	}
	return nil
}

func (*testAdapter) SavePolicy(model.Model) error { return nil }

func (a *testAdapter) AddPolicy(_ string, ptype string, rule []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.addCalls++
	a.rules = append(a.rules, append([]string{ptype}, rule...))
	return nil
}

func (a *testAdapter) RemovePolicy(_ string, ptype string, rule []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.removeCalls++
	want := append([]string{ptype}, rule...)
	for i, existing := range a.rules {
		if equalRule(existing, want) {
			a.rules = append(a.rules[:i], a.rules[i+1:]...)
			break
		}
	}
	return nil
}

func (a *testAdapter) RemoveFilteredPolicy(_ string, ptype string, fieldIndex int, fieldValues ...string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.removeCalls++
	kept := a.rules[:0]
	for _, existing := range a.rules {
		if existing[0] != ptype || !matchesFilter(existing[1:], fieldIndex, fieldValues) {
			kept = append(kept, existing)
		}
	}
	a.rules = kept
	return nil
}

func (a *testAdapter) setRules(rules ...[]string) {
	a.mu.Lock()
	a.rules = cloneRules(rules)
	a.mu.Unlock()
}

func (a *testAdapter) setLoadError(err error) {
	a.mu.Lock()
	a.loadErr = err
	a.mu.Unlock()
}

func (a *testAdapter) counts() (loads, adds int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.loadCalls, a.addCalls
}

func cloneRules(rules [][]string) [][]string {
	out := make([][]string, len(rules))
	for i := range rules {
		out[i] = append([]string(nil), rules[i]...)
	}
	return out
}

func equalRule(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func matchesFilter(rule []string, fieldIndex int, values []string) bool {
	for i, value := range values {
		if value != "" && (fieldIndex+i >= len(rule) || rule[fieldIndex+i] != value) {
			return false
		}
	}
	return true
}

type testWatcher struct {
	mu          sync.Mutex
	callback    func(string)
	callbackErr error
	updates     atomic.Int32
	closes      atomic.Int32
}

var _ persist.Watcher = (*testWatcher)(nil)

func (w *testWatcher) SetUpdateCallback(callback func(string)) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.callbackErr != nil {
		return w.callbackErr
	}
	w.callback = callback
	return nil
}

func (w *testWatcher) Update() error {
	w.updates.Add(1)
	return nil
}

func (w *testWatcher) Close() { w.closes.Add(1) }

func (w *testWatcher) trigger(message string) {
	w.mu.Lock()
	callback := w.callback
	w.mu.Unlock()
	if callback != nil {
		callback(message)
	}
}

type watcherResult struct {
	watcher persist.Watcher
	err     error
}

type testWatcherFactory struct {
	mu       sync.Mutex
	identity string
	results  []watcherResult
	opens    int
	seen     []*casbinlib.SyncedEnforcer
}

func (f *testWatcherFactory) Open(enforcer *casbinlib.SyncedEnforcer) (persist.Watcher, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	f.seen = append(f.seen, enforcer)
	if len(f.results) == 0 {
		return nil, errors.New("no watcher result")
	}
	result := f.results[0]
	f.results = f.results[1:]
	return result.watcher, result.err
}

func (f *testWatcherFactory) openCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens
}

func providerConfig(withWatcher bool) Config {
	cfg := DefaultConfig()
	cfg.Adapter = ProviderRef{Plugin: "adapter-provider", Instance: "writer"}
	if withWatcher {
		cfg.Watcher = ProviderRef{Plugin: "watcher-provider", Instance: "events"}
	}
	return cfg
}

func buildProviderPluginForTest(t *testing.T, cfg Config, adapter *testAdapter, factory WatcherFactory) *Plugin {
	t.Helper()
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatalf("normalizeConfig() error = %v", err)
	}
	p, err := newProviderPlugin(normalized, &testAdapterProvider{adapter: adapter}, factory, nil)
	if err != nil {
		t.Fatalf("newProviderPlugin() error = %v", err)
	}
	return p
}

func TestPlannedDefinitionSelectsExactProviderInstances(t *testing.T) {
	adapterDefinition := plugin.Define(
		"adapter-provider",
		func(ctx plugin.BuildContext) (*testAdapterProvider, error) {
			return &testAdapterProvider{adapter: &identityAdapter{identity: ctx.Identity().String()}}, nil
		},
		plugin.Options[*testAdapterProvider]{
			Instances: plugin.MultipleInstances,
			Exports: plugin.Contracts(
				plugin.ExportAs[AdapterProvider](func(value *testAdapterProvider) AdapterProvider { return value }),
			),
		},
	)
	watcherDefinition := plugin.Define(
		"watcher-provider",
		func(ctx plugin.BuildContext) (*testWatcherFactory, error) {
			return &testWatcherFactory{
				identity: ctx.Identity().String(),
				results:  []watcherResult{{watcher: &testWatcher{}}},
			}, nil
		},
		plugin.Options[*testWatcherFactory]{
			Instances: plugin.MultipleInstances,
			Exports: plugin.Contracts(
				plugin.ExportAs[WatcherFactory](func(value *testWatcherFactory) WatcherFactory { return value }),
			),
		},
	)

	environment := providerEnvironment(t, map[string]any{
		"adapter-provider": map[string]any{"writer": map[string]any{}, "reader": map[string]any{}},
		"watcher-provider": map[string]any{"events": map[string]any{}, "other": map[string]any{}},
		"casbin": map[string]any{
			"adapter": map[string]any{"plugin": "adapter-provider", "instance": "writer"},
			"watcher": map[string]any{"plugin": "watcher-provider", "instance": "events"},
		},
	})
	built, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{plugin.BundleOf(adapterDefinition, watcherDefinition), Bundle()},
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	constructed, err := assembly.Construct(built, assembly.ConstructOptions{})
	if err != nil {
		t.Fatalf("Construct() error = %v", err)
	}
	instance, ok := constructed.Instance(plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
	if !ok {
		t.Fatal("casbin instance was not constructed")
	}
	p := instance.Primary().(*Plugin)
	adapter, ok := p.state.enforcer.GetAdapter().(*identityAdapter)
	if !ok || adapter.identity != "adapter-provider[writer]" {
		t.Fatalf("selected adapter = %#v, want adapter-provider[writer]", adapter)
	}
	watcher, ok := p.state.watcherFactory.(*testWatcherFactory)
	if !ok || watcher.identity != "watcher-provider[events]" {
		t.Fatalf("selected watcher = %#v, want watcher-provider[events]", watcher)
	}
	if !instance.HasStart() || !instance.HasStop() {
		t.Fatalf("casbin lifecycle = (Start %v, Stop %v), want both", instance.HasStart(), instance.HasStop())
	}
	if err := instance.InvokeStart(); err == nil || !strings.Contains(err.Error(), "non-nil plugin context") {
		t.Fatalf("InvokeStart() without a context error = %v", err)
	}

	missing := providerEnvironment(t, map[string]any{
		"adapter-provider": map[string]any{"reader": map[string]any{}},
		"casbin": map[string]any{
			"adapter": map[string]any{"plugin": "adapter-provider", "instance": "writer"},
		},
	})
	_, err = assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{plugin.BundleOf(adapterDefinition), Bundle()},
		Env:     missing,
	})
	if err == nil || !strings.Contains(err.Error(), "adapter-provider[writer]") {
		t.Fatalf("BuildPlan() missing exact provider error = %v", err)
	}

	missingWatcher := providerEnvironment(t, map[string]any{
		"adapter-provider": map[string]any{"writer": map[string]any{}},
		"watcher-provider": map[string]any{"other": map[string]any{}},
		"casbin": map[string]any{
			"adapter": map[string]any{"plugin": "adapter-provider", "instance": "writer"},
			"watcher": map[string]any{"plugin": "watcher-provider", "instance": "events"},
		},
	})
	_, err = assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{plugin.BundleOf(adapterDefinition, watcherDefinition), Bundle()},
		Env:     missingWatcher,
	})
	if err == nil || !strings.Contains(err.Error(), "watcher-provider[events]") {
		t.Fatalf("BuildPlan() missing exact watcher error = %v", err)
	}
}

type identityAdapter struct{ identity string }

func (*identityAdapter) LoadPolicy(model.Model) error                              { return nil }
func (*identityAdapter) SavePolicy(model.Model) error                              { return nil }
func (*identityAdapter) AddPolicy(string, string, []string) error                  { return nil }
func (*identityAdapter) RemovePolicy(string, string, []string) error               { return nil }
func (*identityAdapter) RemoveFilteredPolicy(string, string, int, ...string) error { return nil }

func providerEnvironment(t *testing.T, plugins map[string]any) *config.Environment {
	t.Helper()
	environment, err := config.NewEnvironment(map[string]any{"plugins": plugins}, "XBC_CASBIN_TEST_UNSET_")
	if err != nil {
		t.Fatalf("NewEnvironment() error = %v", err)
	}
	return environment
}

func TestProviderAdapterLoadIsDelayedUntilStartAndAutosaveIsEnabled(t *testing.T) {
	adapter := &testAdapter{}
	adapter.setRules([]string{"p", "alice", "reports:read"})
	p := buildProviderPluginForTest(t, providerConfig(false), adapter, nil)
	if loads, _ := adapter.counts(); loads != 0 {
		t.Fatalf("adapter LoadPolicy calls during construction = %d, want 0", loads)
	}

	enforcer, ok := p.Enforcer()
	if !ok || enforcer.GetAdapter() != adapter {
		t.Fatal("external adapter was not attached during construction")
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
		t.Fatalf("adapter LoadPolicy calls after Start = %d, want 1", loads)
	}
	allowed, err := enforcer.Enforce("alice", "reports:read")
	assertEnforce(t, allowed, err, true)

	added, err := enforcer.AddPolicy("bob", "reports:read")
	if err != nil || !added {
		t.Fatalf("AddPolicy() = (%v, %v), want (true, nil)", added, err)
	}
	if _, adds := adapter.counts(); adds != 1 {
		t.Fatalf("adapter AddPolicy calls = %d, want autosave call", adds)
	}
}

func TestProviderReloadIntervalLoadsExternalAdapter(t *testing.T) {
	adapter := &testAdapter{}
	adapter.setRules([]string{"p", "alice", "reports:read"})
	cfg := providerConfig(false)
	cfg.ReloadInterval = 5 * time.Millisecond
	p := buildProviderPluginForTest(t, cfg, adapter, nil)
	host := newTestHost()
	if err := p.start(testContext(host)); err != nil {
		host.close()
		t.Fatalf("start() error = %v", err)
	}
	t.Cleanup(func() {
		_ = p.Stop(context.Background())
		host.close()
	})
	adapter.setRules([]string{"p", "bob", "reports:read"})
	enforcer, _ := p.Enforcer()
	waitFor(t, time.Second, func() bool {
		allowed, err := enforcer.Enforce("bob", "reports:read")
		return err == nil && allowed
	}, "periodic reload did not load the external adapter")
}

func TestWatcherIsAttachedBeforeInitialLoadAndCallbackReloadsPolicy(t *testing.T) {
	adapter := &testAdapter{}
	adapter.setRules([]string{"p", "alice", "reports:read"})
	watcher := &testWatcher{}
	factory := &testWatcherFactory{results: []watcherResult{{watcher: watcher}}}
	p := buildProviderPluginForTest(t, providerConfig(true), adapter, factory)
	host := newTestHost()
	if err := p.start(testContext(host)); err != nil {
		host.close()
		t.Fatalf("start() error = %v", err)
	}
	t.Cleanup(func() {
		_ = p.Stop(context.Background())
		host.close()
	})
	if factory.openCount() != 1 {
		t.Fatalf("watcher Open calls = %d, want 1", factory.openCount())
	}
	if watcher.callback == nil {
		t.Fatal("watcher callback was not attached")
	}

	adapter.setRules([]string{"p", "bob", "reports:read"})
	watcher.trigger("changed")
	enforcer, _ := p.Enforcer()
	allowed, err := enforcer.Enforce("alice", "reports:read")
	assertEnforce(t, allowed, err, false)
	allowed, err = enforcer.Enforce("bob", "reports:read")
	assertEnforce(t, allowed, err, true)

	added, err := enforcer.AddPolicy("carol", "reports:read")
	if err != nil || !added {
		t.Fatalf("AddPolicy() = (%v, %v)", added, err)
	}
	if got := watcher.updates.Load(); got != 1 {
		t.Fatalf("watcher Update calls = %d, want 1", got)
	}
}

func TestWatcherStartupFailuresRollbackAndPermitRetry(t *testing.T) {
	t.Run("open", func(t *testing.T) {
		adapter := &testAdapter{}
		good := &testWatcher{}
		factory := &testWatcherFactory{results: []watcherResult{
			{err: errors.New("redis unavailable")},
			{watcher: good},
		}}
		p := buildProviderPluginForTest(t, providerConfig(true), adapter, factory)
		host := newTestHost()
		defer host.close()
		if err := p.start(testContext(host)); err == nil || !strings.Contains(err.Error(), "open watcher") {
			t.Fatalf("start() open error = %v", err)
		}
		if loads, _ := adapter.counts(); loads != 0 {
			t.Fatalf("adapter loaded after watcher open failure: %d", loads)
		}
		if err := p.start(testContext(host)); err != nil {
			t.Fatalf("retry start() error = %v", err)
		}
		if err := p.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := good.closes.Load(); got != 1 {
			t.Fatalf("successful watcher closes = %d, want 1", got)
		}
	})

	t.Run("callback", func(t *testing.T) {
		adapter := &testAdapter{}
		bad := &testWatcher{callbackErr: errors.New("callback refused")}
		good := &testWatcher{}
		factory := &testWatcherFactory{results: []watcherResult{{watcher: bad}, {watcher: good}}}
		p := buildProviderPluginForTest(t, providerConfig(true), adapter, factory)
		host := newTestHost()
		defer host.close()
		if err := p.start(testContext(host)); err == nil || !strings.Contains(err.Error(), "attach watcher") {
			t.Fatalf("start() callback error = %v", err)
		}
		if got := bad.closes.Load(); got != 1 {
			t.Fatalf("failed watcher closes = %d, want 1", got)
		}
		if loads, _ := adapter.counts(); loads != 0 {
			t.Fatalf("adapter loaded after callback failure: %d", loads)
		}
		if err := p.start(testContext(host)); err != nil {
			t.Fatalf("retry start() error = %v", err)
		}
		if err := p.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := bad.closes.Load(); got != 1 {
			t.Fatalf("failed watcher closed again: %d", got)
		}
		if got := good.closes.Load(); got != 1 {
			t.Fatalf("successful watcher closes = %d, want 1", got)
		}
	})

	t.Run("initial load", func(t *testing.T) {
		adapter := &testAdapter{}
		adapter.setLoadError(errors.New("table missing"))
		first, second := &testWatcher{}, &testWatcher{}
		factory := &testWatcherFactory{results: []watcherResult{{watcher: first}, {watcher: second}}}
		p := buildProviderPluginForTest(t, providerConfig(true), adapter, factory)
		host := newTestHost()
		defer host.close()
		if err := p.start(testContext(host)); err == nil || !strings.Contains(err.Error(), "initial adapter policy load") {
			t.Fatalf("start() load error = %v", err)
		}
		if got := first.closes.Load(); got != 1 {
			t.Fatalf("watcher closes after load failure = %d, want 1", got)
		}
		adapter.setLoadError(nil)
		if err := p.start(testContext(host)); err != nil {
			t.Fatalf("retry start() error = %v", err)
		}
		if err := p.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := first.closes.Load(); got != 1 {
			t.Fatalf("failed-start watcher closed again: %d", got)
		}
		if got := second.closes.Load(); got != 1 {
			t.Fatalf("retry watcher closes = %d, want 1", got)
		}
	})
}

func TestConcurrentStartAndStopClosesLocallyOpenedWatcherExactlyOnce(t *testing.T) {
	adapter := &testAdapter{}
	watcher := &testWatcher{}
	opened := make(chan struct{})
	release := make(chan struct{})
	factory := watcherFactoryFunc(func(*casbinlib.SyncedEnforcer) (persist.Watcher, error) {
		close(opened)
		<-release
		return watcher, nil
	})
	p := buildProviderPluginForTest(t, providerConfig(true), adapter, factory)
	host := newTestHost()
	defer host.close()

	startErr := make(chan error, 1)
	go func() { startErr <- p.start(testContext(host)) }()
	<-opened
	stopErr := make(chan error, 1)
	go func() { stopErr <- p.Stop(context.Background()) }()
	waitFor(t, time.Second, p.stopped.Load, "Stop did not publish its stopped state")
	close(release)

	if err := <-startErr; !errors.Is(err, ErrStopped) {
		t.Fatalf("start() during Stop = %v, want ErrStopped", err)
	}
	if err := <-stopErr; err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if got := watcher.closes.Load(); got != 1 {
		t.Fatalf("locally opened watcher Close calls = %d, want exactly 1", got)
	}
}

type watcherFactoryFunc func(*casbinlib.SyncedEnforcer) (persist.Watcher, error)

func (f watcherFactoryFunc) Open(enforcer *casbinlib.SyncedEnforcer) (persist.Watcher, error) {
	return f(enforcer)
}

func TestConcurrentStopClosesWatcherExactlyOnce(t *testing.T) {
	adapter := &testAdapter{}
	watcher := &testWatcher{}
	factory := &testWatcherFactory{results: []watcherResult{{watcher: watcher}}}
	p := buildProviderPluginForTest(t, providerConfig(true), adapter, factory)
	host := newTestHost()
	if err := p.start(testContext(host)); err != nil {
		host.close()
		t.Fatal(err)
	}

	const callers = 64
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- p.Stop(context.Background())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
	}
	host.close()
	if got := watcher.closes.Load(); got != 1 {
		t.Fatalf("watcher Close calls = %d, want exactly 1", got)
	}
}

func TestNewRejectsConfiguredProviders(t *testing.T) {
	cfg := providerConfig(false)
	_, err := New(cfg)
	if err == nil || !strings.Contains(err.Error(), "Definition/Bundle composition") {
		t.Fatalf("New() provider error = %v", err)
	}
}

func TestProviderConstructionRejectsNilCapabilities(t *testing.T) {
	cfg := providerConfig(false)
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		provider AdapterProvider
		want     string
	}{
		{name: "provider", provider: (*testAdapterProvider)(nil), want: "nil capability"},
		{name: "adapter", provider: &testAdapterProvider{}, want: "nil adapter"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := newProviderPlugin(normalized, test.provider, nil, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("newProviderPlugin() error = %v, want %q", err, test.want)
			}
		})
	}
}

func (a *identityAdapter) String() string { return fmt.Sprintf("identityAdapter(%s)", a.identity) }
