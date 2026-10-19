package saml

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/xbcio/xbc/config"
	federationcore "github.com/xbcio/xbc/extensions/authentication/federation"
	samlcore "github.com/xbcio/xbc/extensions/authentication/saml"
	sessioncore "github.com/xbcio/xbc/extensions/authentication/session"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
	transportweb "github.com/xbcio/xbc/transport/web"
	federationweb "github.com/xbcio/xbc/transport/web/extensions/authentication/federation"
)

// --- fake login ------------------------------------------------------------

type stubLogin struct{}

func (*stubLogin) Begin(context.Context, *transportweb.Ctx, federationweb.BeginRequest) (federationweb.Transaction, error) {
	return federationweb.Transaction{}, nil
}

func (*stubLogin) Consume(context.Context, *transportweb.Ctx, string, string) (federationweb.Transaction, bool, error) {
	return federationweb.Transaction{}, false, nil
}

func (*stubLogin) Complete(context.Context, *transportweb.Ctx, federationcore.ExternalIdentity, string) error {
	return nil
}

func (*stubLogin) Fail(*transportweb.Ctx, federationweb.ErrorCode) {}

func (*stubLogin) ReturnTo(raw string) string { return raw }

func (*stubLogin) SetIfAbsent(context.Context, string, string, time.Duration) (bool, error) {
	return false, nil
}

var _ federationweb.Login = (*stubLogin)(nil)

// --- definition ------------------------------------------------------------

func TestDefinitionIsCanonicalAndExportsTheRouteContributor(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero {
		t.Fatal("Definition() returned a zero handle")
	}
	if Definition() != Definition() {
		t.Fatal("Definition() returned different handles")
	}
	descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(Definition()))
	if !ok {
		t.Fatal("Definition() returned a zero handle")
	}
	if descriptor.Key != pluginmodel.Key(Key) {
		t.Fatalf("Definition key = %q, want %q", descriptor.Key, Key)
	}
	want := []reflect.Type{reflect.TypeOf((*transportweb.RouteContributor)(nil)).Elem()}
	var got []reflect.Type
	for _, contract := range descriptor.Contracts {
		got = append(got, contract.Type)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Definition contracts = %v, want %v", got, want)
	}
	if descriptor.Activation.Kind != pluginmodel.ActivationConfigured || descriptor.Activation.Path != "plugins."+Key.String() {
		t.Fatalf("Activation = %#v, want WhenConfigured on plugins.%s", descriptor.Activation, Key)
	}
}

func TestBundleSelectsTheSharedBaseAndTheSessionPlugins(t *testing.T) {
	if reflect.DeepEqual(Bundle(), plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(Bundle(), Bundle()) {
		t.Fatal("Bundle() returned different composition content")
	}
	var got []pluginmodel.Definition
	for _, entry := range pluginmodel.BundleEntries(pluginmodel.Bundle(Bundle())) {
		got = append(got, entry.Definition)
	}
	// federationweb.Bundle() itself selects the Web session adapter between
	// the neutral session plugin and the shared base, so the flattened order
	// is sessioncore, session-http, federation-http, saml-http. Comparing the
	// flattened list against a four-entry expectation keeps a dropped session
	// plugin from passing silently.
	want := []pluginmodel.Definition{
		pluginmodel.Definition(sessioncore.Definition()),
		bundleDefinition(t, "session-http"),
		pluginmodel.Definition(federationweb.Definition()),
		pluginmodel.Definition(Definition()),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Bundle() definitions = %v, want %v", got, want)
	}
}

// bundleDefinition finds one definition in Bundle() by key.
func bundleDefinition(t *testing.T, key string) pluginmodel.Definition {
	t.Helper()
	for _, entry := range pluginmodel.BundleEntries(pluginmodel.Bundle(Bundle())) {
		descriptor, ok := pluginmodel.DescribeDefinition(entry.Definition)
		if ok && string(descriptor.Key) == key {
			return entry.Definition
		}
	}
	t.Fatalf("Bundle() does not contain %q", key)
	return pluginmodel.Definition{}
}

// --- direct construction ---------------------------------------------------

func newTestRegistry(t *testing.T) *federationcore.Registry[*samlcore.Provider] {
	t.Helper()
	return samlcore.NewRegistry()
}

func TestNewRejectsUnusableDependencies(t *testing.T) {
	var typedNilLogin *stubLogin
	var typedNilRegistry *federationcore.Registry[*samlcore.Provider]
	for name, testCase := range map[string]struct {
		registry *federationcore.Registry[*samlcore.Provider]
		login    federationweb.Login
	}{
		"nil registry":       {nil, &stubLogin{}},
		"typed nil registry": {typedNilRegistry, &stubLogin{}},
		"nil login":          {newTestRegistry(t), nil},
		"typed nil login":    {newTestRegistry(t), typedNilLogin},
	} {
		t.Run(name, func(t *testing.T) {
			if adapter, err := New(DefaultConfig(), testCase.registry, testCase.login); err == nil || adapter != nil {
				t.Fatalf("New() = %#v, %v; want nil and an error", adapter, err)
			}
		})
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	if _, err := New(Config{Prefix: "relative"}, newTestRegistry(t), &stubLogin{}); err == nil {
		t.Fatal("New() with a relative prefix = nil error")
	}
}

func TestNewNormalizesTheConfiguredPrefix(t *testing.T) {
	adapter, err := New(Config{Prefix: "/api/v1/auth/saml/"}, newTestRegistry(t), &stubLogin{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if adapter.config.prefix != "/api/v1/auth/saml" {
		t.Fatalf("prefix = %q", adapter.config.prefix)
	}
}

// --- assembly --------------------------------------------------------------

type assemblyResolver struct{}

func (*assemblyResolver) Resolve(context.Context, federationcore.ExternalIdentity) (federationcore.Resolution, error) {
	return federationcore.Refused(federationcore.RefusalUnbound), nil
}

func resolverProducer(key string) plugin.Definition {
	return plugin.Define(
		plugin.Key(key),
		func(plugin.BuildContext) (*assemblyResolver, error) { return &assemblyResolver{}, nil },
		plugin.Options[*assemblyResolver]{
			Exports: plugin.Contracts(
				plugin.ExportAs[federationcore.IdentityResolver](func(value *assemblyResolver) federationcore.IdentityResolver { return value }),
			),
		},
	)
}

// registryProducer registers the application-owned SAML registry as a
// definition's primary value, which is how an application exposes it to the
// adapter's concrete-pointer input.
func registryProducer(key string, registry *federationcore.Registry[*samlcore.Provider]) plugin.Definition {
	return plugin.Define(
		plugin.Key(key),
		func(plugin.BuildContext) (*federationcore.Registry[*samlcore.Provider], error) {
			return registry, nil
		},
	)
}

func adapterEnvironment(t *testing.T, plugins map[string]any) *config.Environment {
	t.Helper()
	environment, err := config.NewEnvironment(map[string]any{"plugins": plugins}, "")
	if err != nil {
		t.Fatal(err)
	}
	return environment
}

func constructAdapter(t *testing.T, bundles []plugin.Bundle, plugins map[string]any) (*Plugin, error) {
	t.Helper()
	built, err := assembly.BuildPlan(assembly.PlanOptions{Bundles: bundles, Env: adapterEnvironment(t, plugins)})
	if err != nil {
		return nil, err
	}
	constructed, err := assembly.Construct(built, assembly.ConstructOptions{})
	if err != nil {
		return nil, err
	}
	instance, found := constructed.Instance(plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
	if !found {
		t.Fatal("the adapter was not constructed")
	}
	adapter, ok := instance.Primary().(*Plugin)
	if !ok {
		t.Fatalf("primary = %T, want *Plugin", instance.Primary())
	}
	return adapter, nil
}

func TestAssemblyBindsTheRegistryAndTheSharedBase(t *testing.T) {
	registry := newTestRegistry(t)
	adapter, err := constructAdapter(t,
		[]plugin.Bundle{plugin.CombineBundles(
			plugin.BundleOf(resolverProducer("test-resolver")),
			plugin.BundleOf(registryProducer("test-registry", registry)),
			Bundle(),
		)},
		map[string]any{
			"session":         map[string]any{},
			"federation-http": map[string]any{"backend": "memory"},
			"saml-http":       map[string]any{},
		},
	)
	if err != nil {
		t.Fatalf("assembly error = %v", err)
	}
	if adapter.registry != registry {
		t.Fatal("assembly did not bind the application registry")
	}
	if _, ok := adapter.login.(*federationweb.Plugin); !ok {
		t.Fatalf("login = %T, want the shared federation base", adapter.login)
	}
	if adapter.config.prefix != "/auth/saml" {
		t.Fatalf("prefix = %q", adapter.config.prefix)
	}
}

func TestAssemblyFailsWithoutTheRegistry(t *testing.T) {
	if _, err := constructAdapter(t,
		[]plugin.Bundle{plugin.CombineBundles(plugin.BundleOf(resolverProducer("test-resolver")), Bundle())},
		map[string]any{
			"session":         map[string]any{},
			"federation-http": map[string]any{"backend": "memory"},
			"saml-http":       map[string]any{},
		},
	); err == nil {
		t.Fatal("assembly without a SAML registry = nil error")
	}
}

func TestAssemblyFailsWithTwoRegistries(t *testing.T) {
	if _, err := constructAdapter(t,
		[]plugin.Bundle{plugin.CombineBundles(
			plugin.BundleOf(resolverProducer("test-resolver")),
			plugin.BundleOf(registryProducer("test-registry-one", newTestRegistry(t))),
			plugin.BundleOf(registryProducer("test-registry-two", newTestRegistry(t))),
			Bundle(),
		)},
		map[string]any{
			"session":         map[string]any{},
			"federation-http": map[string]any{"backend": "memory"},
			"saml-http":       map[string]any{},
		},
	); err == nil {
		t.Fatal("assembly with two SAML registries = nil error")
	}
}

func TestAssemblyFailsWithoutTheSharedBase(t *testing.T) {
	// plugins.federation-http is left out, so the shared base the adapter
	// requires stays dormant.
	if _, err := constructAdapter(t,
		[]plugin.Bundle{plugin.CombineBundles(
			plugin.BundleOf(resolverProducer("test-resolver")),
			plugin.BundleOf(registryProducer("test-registry", newTestRegistry(t))),
			Bundle(),
		)},
		map[string]any{
			"session":   map[string]any{},
			"saml-http": map[string]any{},
		},
	); err == nil {
		t.Fatal("assembly without the shared federation base = nil error")
	}
}

func TestAssemblyRejectsAnInvalidPrefix(t *testing.T) {
	if _, err := constructAdapter(t,
		[]plugin.Bundle{plugin.CombineBundles(
			plugin.BundleOf(resolverProducer("test-resolver")),
			plugin.BundleOf(registryProducer("test-registry", newTestRegistry(t))),
			Bundle(),
		)},
		map[string]any{
			"session":         map[string]any{},
			"federation-http": map[string]any{"backend": "memory"},
			"saml-http":       map[string]any{"prefix": "/auth/:tenant"},
		},
	); err == nil {
		t.Fatal("assembly with a route-parameter prefix = nil error")
	}
}
