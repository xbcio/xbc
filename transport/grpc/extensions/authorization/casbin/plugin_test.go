package casbin

import (
	"context"
	"reflect"
	"testing"

	casbinlib "github.com/casbin/casbin/v2"

	"github.com/xbcio/xbc/extensions/authentication"
	casbincore "github.com/xbcio/xbc/extensions/authorization/casbin"
	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
	grpctransport "github.com/xbcio/xbc/transport/grpc"
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

// TestBundleSelectsTheNeutralEngine pins the production spelling: a
// composition root that selects casbin.Bundle() from this package gets the
// neutral engine too, so it never has to know the enforcement was split out.
func TestBundleSelectsTheNeutralEngine(t *testing.T) {
	var got []pluginmodel.Definition
	for _, entry := range pluginmodel.BundleEntries(pluginmodel.Bundle(Bundle())) {
		got = append(got, entry.Definition)
	}
	want := []pluginmodel.Definition{
		pluginmodel.Definition(casbincore.Definition()),
		pluginmodel.Definition(Definition()),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Bundle() definitions = %v, want the neutral engine then the gRPC interceptor", got)
	}
}

// TestDefinitionExportsBothInterceptorChains keeps the one value contributing
// to both chains: a composition root that selects this Definition must not be
// able to get a unary-only or stream-only interceptor.
func TestDefinitionExportsBothInterceptorChains(t *testing.T) {
	descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(Definition()))
	if !ok {
		t.Fatal("Definition() returned a zero handle")
	}
	unary := reflect.TypeOf((*grpctransport.UnaryInterceptor)(nil)).Elem()
	stream := reflect.TypeOf((*grpctransport.StreamInterceptor)(nil)).Elem()
	if len(descriptor.Contracts) != 2 ||
		descriptor.Contracts[0].Type != unary ||
		descriptor.Contracts[1].Type != stream {
		t.Fatalf("Definition contracts = %+v, want exactly the unary then the stream interceptor", descriptor.Contracts)
	}
}

// TestOrderAndPrincipalMarkerPinTheAuthenticationDependency is the ordering
// contract in one place: the phase alone would leave same-phase order to the
// identity tie-break, and the RequiresPrincipal marker is what the framework
// turns into a required predecessor edge on the authentication stage.
func TestOrderAndPrincipalMarkerPinTheAuthenticationDependency(t *testing.T) {
	var value any = new(Plugin)
	if _, ok := value.(authentication.RequiresPrincipal); !ok {
		t.Fatal("*Plugin must implement authentication.RequiresPrincipal; the framework pin is its only ordering edge to the authentication stage")
	}
	p, _ := initializedPlugin(t, nil, DefaultConfig())
	order := p.Order()
	if order.Phase != grpctransport.PhaseAuth {
		t.Fatalf("Order().Phase = %v, want PhaseAuth", order.Phase)
	}
	if len(order.Before) != 0 || len(order.After) != 0 {
		t.Fatalf("Order() = %+v, want the framework pin as the only authentication edge", order)
	}
	if p.UnaryInterceptor() == nil || p.StreamInterceptor() == nil {
		t.Fatal("interceptor chains = nil")
	}
}

// foreignProvider is an EnforcerProvider outside the neutral engine, so a test
// can pin what the interceptor does with a foreign declaration.
type foreignProvider struct {
	enforcer   *casbinlib.SyncedEnforcer
	active     bool
	convention casbincore.RequestConvention
}

func (f foreignProvider) Enforcer() (*casbinlib.SyncedEnforcer, bool) { return f.enforcer, f.active }

func (f foreignProvider) RequestConvention() casbincore.RequestConvention { return f.convention }

func activeEnforcer(t *testing.T) *casbinlib.SyncedEnforcer {
	t.Helper()
	engine, err := casbincore.New(casbincore.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Stop(context.Background()) })
	enforcer, active := engine.Enforcer()
	if !active || enforcer == nil {
		t.Fatal("the fresh engine reported no active enforcer")
	}
	return enforcer
}

func TestNewRejectsMissingEnforcerProvider(t *testing.T) {
	if p, err := New(nil, DefaultConfig()); err == nil || p != nil {
		t.Fatalf("New(nil) = %#v, %v; want nil interceptor and error", p, err)
	}
	var typedNil *casbincore.Plugin
	if p, err := New(typedNil, DefaultConfig()); err == nil || p != nil {
		t.Fatalf("New(typed nil) = %#v, %v; want nil interceptor and error", p, err)
	}
}

func TestNewRejectsStoppedEngine(t *testing.T) {
	engine, err := casbincore.New(casbincore.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p, err := New(engine, DefaultConfig()); err == nil || p != nil {
		t.Fatalf("New(stopped engine) = %#v, %v; want nil interceptor and error", p, err)
	}
}

func TestNewRejectsUnsupportedRequestConvention(t *testing.T) {
	provider := foreignProvider{enforcer: activeEnforcer(t), active: true, convention: "role_action"}
	if p, err := New(provider, DefaultConfig()); err == nil || p != nil {
		t.Fatalf("New(bogus convention) = %#v, %v; want nil interceptor and error", p, err)
	}
}

func TestNewAcceptsAProvidersDeclaredConvention(t *testing.T) {
	for _, convention := range []casbincore.RequestConvention{casbincore.ConventionRoutePermission, casbincore.ConventionPathMethod} {
		provider := foreignProvider{enforcer: activeEnforcer(t), active: true, convention: convention}
		p, err := New(provider, DefaultConfig())
		if err != nil {
			t.Fatalf("New(%s) error = %v", convention, err)
		}
		if p.state.convention != convention {
			t.Fatalf("state convention = %q, want %q", p.state.convention, convention)
		}
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	_, engine := initializedPlugin(t, nil, DefaultConfig())
	enforcer, active := engine.Enforcer()
	if !active {
		t.Fatal("engine is not active")
	}
	provider := foreignProvider{enforcer: enforcer, active: true, convention: casbincore.ConventionRoutePermission}
	if p, err := New(provider, Config{MissingPermission: "skip"}); err == nil || p != nil {
		t.Fatalf("New(bogus missing_permission) = %#v, %v; want nil interceptor and error", p, err)
	}
}

func TestLifecycleOperationErrorsAndIdempotentStop(t *testing.T) {
	p, _ := initializedPlugin(t, nil, DefaultConfig())

	if err := p.start(nil); err == nil {
		t.Fatal("start(nil) succeeded")
	}
	host := newTestHost()
	t.Cleanup(host.close)
	if err := p.start(testContext(host)); err == nil {
		t.Fatal("a second start succeeded")
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
	if err := p.start(testContext(host)); err == nil {
		t.Fatal("start after stop succeeded")
	}
}
