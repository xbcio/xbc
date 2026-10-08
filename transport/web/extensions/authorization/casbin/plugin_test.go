package casbin

import (
	"context"
	"reflect"
	"strings"
	"testing"

	casbinlib "github.com/casbin/casbin/v2"

	casbincore "github.com/xbcio/xbc/extensions/authorization/casbin"
	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
	"github.com/xbcio/xbc/transport/web"
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

// TestDefinitionExportsExactlyTheWebMiddlewareContract pins the split's
// boundary from the middleware side. This product contributes route
// enforcement and nothing else: EnforcerProvider and rbac.Backend belong to
// the neutral engine (see extensions/authorization/casbin), and re-exporting
// either here would put the policy backend back on the Web stack's contract
// surface.
func TestDefinitionExportsExactlyTheWebMiddlewareContract(t *testing.T) {
	descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(Definition()))
	if !ok {
		t.Fatal("Definition() returned a zero handle")
	}

	want := reflect.TypeOf((*web.Middleware)(nil)).Elem()
	if len(descriptor.Contracts) != 1 {
		t.Fatalf("Definition declares %d contracts, want exactly 1 (web.Middleware): %+v", len(descriptor.Contracts), descriptor.Contracts)
	}
	if got := descriptor.Contracts[0].Type; got != want {
		t.Fatalf("Definition contracts = %v, want %v", got, want)
	}
}

func TestNewAppliesDefaultsAndMiddlewareContract(t *testing.T) {
	p, _ := initializedPlugin(t, nil, nil)
	if p.Order().Phase != web.PhaseAuth {
		t.Fatalf("Order().Phase = %v, want PhaseAuth", p.Order().Phase)
	}
	after := p.Order().After
	if len(after) != 1 || after[0] != web.Require(web.AuthenticationMiddlewareKey) {
		t.Fatalf("Order().After = %v, want [Require(authentication-middleware)]", after)
	}
	if p.Handler() == nil {
		t.Fatal("Handler() = nil")
	}
}

// TestOrderRequiresAuthenticationMiddleware locks the hard-reference: a soft
// Prefer degrades silently into lexicographic tie-break the moment its target
// plugin's Order() stops naming it, and that yields a 403 at runtime with no
// compile or test failure to point at it. A mutation back to
// web.Prefer(web.AuthenticationMiddlewareKey) must fail this test.
func TestOrderRequiresAuthenticationMiddleware(t *testing.T) {
	t.Parallel()

	order := (&Plugin{}).Order()
	var found bool
	for _, ref := range order.After {
		if ref == web.Require(web.AuthenticationMiddlewareKey) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("authorization must hard-require the authentication middleware; " +
			"a soft Prefer degrades silently into lexicographic tie-break and yields 403")
	}
}

func TestNewRejectsMissingEnforcerProvider(t *testing.T) {
	t.Parallel()

	if _, err := New(nil, DefaultConfig()); err == nil || !strings.Contains(err.Error(), "enforcer provider") {
		t.Fatalf("New(nil provider) error = %v, want enforcer provider rejection", err)
	}
}

// foreignProvider lets a test act as an EnforcerProvider the neutral engine
// never built, which is exactly the case New's construction-time checks exist
// to judge.
type foreignProvider struct {
	enforcer   *casbinlib.SyncedEnforcer
	active     bool
	convention casbincore.RequestConvention
}

func (f foreignProvider) Enforcer() (*casbinlib.SyncedEnforcer, bool) { return f.enforcer, f.active }

func (f foreignProvider) RequestConvention() casbincore.RequestConvention { return f.convention }

var _ casbincore.EnforcerProvider = foreignProvider{}

// TestNewRejectsStoppedEngine pins the provider-side half of construction:
// EnforcerProvider reports no active enforcer once the engine's Stop has run,
// and a middleware built against that provider could never enforce anything,
// so New refuses it instead of producing a plugin that 403s every route.
func TestNewRejectsStoppedEngine(t *testing.T) {
	t.Parallel()

	engine, err := casbincore.New(casbincore.DefaultConfig())
	if err != nil {
		t.Fatalf("casbincore.New() error = %v", err)
	}
	if err := engine.Stop(context.Background()); err != nil {
		t.Fatalf("engine Stop() error = %v", err)
	}

	if _, err := New(engine, DefaultConfig()); err == nil || !strings.Contains(err.Error(), "no active enforcer") {
		t.Fatalf("New(stopped engine) error = %v, want no-active-enforcer rejection", err)
	}
}

// TestNewRejectsUnsupportedRequestConvention pins the other construction
// refusal. The engine never reports a convention outside its own two, so only
// a foreign EnforcerProvider can declare something else -- and the middleware
// must refuse it at construction rather than leave every matched request to
// deny itself at runtime through an empty request tuple. The zero value is the
// realistic form: an implementation that never filled the method in.
func TestNewRejectsUnsupportedRequestConvention(t *testing.T) {
	t.Parallel()

	engine, err := casbincore.New(casbincore.DefaultConfig())
	if err != nil {
		t.Fatalf("casbincore.New() error = %v", err)
	}
	t.Cleanup(func() { _ = engine.Stop(context.Background()) })
	enforcer, active := engine.Enforcer()
	if !active || enforcer == nil {
		t.Fatal("engine did not hand out an enforcer")
	}

	for _, convention := range []casbincore.RequestConvention{"", "scope_based"} {
		provider := foreignProvider{enforcer: enforcer, active: true, convention: convention}
		if _, err := New(provider, DefaultConfig()); err == nil || !strings.Contains(err.Error(), "unsupported request convention") {
			t.Fatalf("New(convention %q) error = %v, want unsupported-convention rejection", convention, err)
		}
	}
}

// TestNewAcceptsAProvidersDeclaredConvention pins the positive half: the
// middleware trusts a foreign provider's reported convention rather than
// re-deriving it from the enforcer's model, which is unsynchronized with
// reloads. The enforcer here is the engine's default (route_permission) while
// the provider reports path_method, and the middleware must believe the
// provider -- the engine's guarantee that the two agree does not extend to
// foreign implementations, and reading the model to check would reintroduce
// exactly the race this contract removes.
func TestNewAcceptsAProvidersDeclaredConvention(t *testing.T) {
	t.Parallel()

	engine, err := casbincore.New(casbincore.DefaultConfig())
	if err != nil {
		t.Fatalf("casbincore.New() error = %v", err)
	}
	t.Cleanup(func() { _ = engine.Stop(context.Background()) })
	enforcer, _ := engine.Enforcer()

	p, err := New(foreignProvider{enforcer: enforcer, active: true, convention: casbincore.ConventionPathMethod}, DefaultConfig())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if p.state.convention != casbincore.ConventionPathMethod {
		t.Fatalf("state.convention = %q, want the provider's declared value", p.state.convention)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	engine, err := casbincore.New(casbincore.DefaultConfig())
	if err != nil {
		t.Fatalf("casbincore.New() error = %v", err)
	}
	t.Cleanup(func() { _ = engine.Stop(context.Background()) })

	cfg := DefaultConfig()
	cfg.MissingPermission = "ignore"
	if _, err := New(engine, cfg); err == nil || !strings.Contains(err.Error(), "missing_permission") {
		t.Fatalf("New() error = %v, want missing_permission failure", err)
	}
}

func TestLifecycleOperationErrorsAndIdempotentStop(t *testing.T) {
	engine, err := casbincore.New(casbincore.DefaultConfig())
	if err != nil {
		t.Fatalf("casbincore.New() error = %v", err)
	}
	t.Cleanup(func() { _ = engine.Stop(context.Background()) })

	p, err := New(engine, DefaultConfig())
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
	if err := p.start(testContext(host)); err == nil {
		t.Fatal("start() after Stop error = nil")
	}
	host.close()
}
