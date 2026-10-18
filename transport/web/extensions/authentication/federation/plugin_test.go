package federation

import (
	"context"
	"reflect"
	"testing"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/config"
	federationcore "github.com/xbcio/xbc/extensions/authentication/federation"
	sessioncore "github.com/xbcio/xbc/extensions/authentication/session"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
	sessionweb "github.com/xbcio/xbc/transport/web/extensions/authentication/session"
)

func TestDefinitionIsCanonicalAndExportsLogin(t *testing.T) {
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
	want := []reflect.Type{reflect.TypeOf((*Login)(nil)).Elem()}
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

func TestBundleSelectsTheSessionPluginsAlongsideTheSharedBase(t *testing.T) {
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
	want := []pluginmodel.Definition{
		pluginmodel.Definition(sessioncore.Definition()),
		pluginmodel.Definition(sessionweb.Definition()),
		pluginmodel.Definition(Definition()),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Bundle() definitions = %v, want the neutral session plugin, the Web session adapter, then the shared base", got)
	}
}

func TestBundleStaysDormantWithoutConfiguration(t *testing.T) {
	environment, err := config.NewEnvironment(map[string]any{}, "")
	if err != nil {
		t.Fatal(err)
	}
	built, err := assembly.BuildPlan(assembly.PlanOptions{Bundles: []plugin.Bundle{Bundle()}, Env: environment})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	constructed, err := assembly.Construct(built, assembly.ConstructOptions{})
	if err != nil {
		t.Fatalf("Construct() error = %v", err)
	}
	if _, found := constructed.Instance(plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance}); found {
		t.Fatal("the shared base was constructed without plugins.federation-http")
	}
}

func TestNewUsesMemoryOnlyWhenExplicitlySelected(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Backend = BackendMemory
	p, err := New(cfg, &stubResolver{}, &fakeSessions{}, testSessionCookie)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, ok := p.store.(*MemoryStore); !ok {
		t.Fatalf("store = %T, want *MemoryStore", p.store)
	}
	if p.sessionCookie != testSessionCookie {
		t.Fatalf("session cookie = %q", p.sessionCookie)
	}
}

func TestNewRequiresTheRedisStoreForDirectConstruction(t *testing.T) {
	if _, err := New(DefaultConfig(), &stubResolver{}, &fakeSessions{}, testSessionCookie); err == nil {
		t.Fatal("New() with the default redis backend and no store = nil error")
	}

	cfg := DefaultConfig()
	cfg.Backend = BackendRedis
	if _, err := New(cfg, &stubResolver{}, &fakeSessions{}, testSessionCookie); err == nil {
		t.Fatal("New() with the redis backend and no store = nil error")
	}

	injected := NewMemoryStore()
	p, err := New(cfg, &stubResolver{}, &fakeSessions{}, testSessionCookie, WithStore(injected))
	if err != nil {
		t.Fatalf("New() with an injected store error = %v", err)
	}
	if p.store != injected {
		t.Fatal("injected store was not used")
	}
}

func TestNewRejectsUnusableDependencies(t *testing.T) {
	var typedNilResolver *stubResolver
	var typedNilSessions *fakeSessions
	var typedNilStore *MemoryStore
	for name, testCase := range map[string]struct {
		resolver federationcore.IdentityResolver
		sessions sessionweb.Manager
		cookie   string
		opts     []Option
	}{
		"nil resolver":       {nil, &fakeSessions{}, testSessionCookie, nil},
		"typed nil resolver": {typedNilResolver, &fakeSessions{}, testSessionCookie, nil},
		"nil sessions":       {&stubResolver{}, nil, testSessionCookie, nil},
		"typed nil sessions": {&stubResolver{}, typedNilSessions, testSessionCookie, nil},
		"empty cookie name":  {&stubResolver{}, &fakeSessions{}, " ", nil},
		"typed nil store":    {&stubResolver{}, &fakeSessions{}, testSessionCookie, []Option{WithStore(typedNilStore)}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Backend = BackendMemory
			if p, err := New(cfg, testCase.resolver, testCase.sessions, testCase.cookie, testCase.opts...); err == nil || p != nil {
				t.Fatalf("New() = %#v, %v; want nil and an error", p, err)
			}
		})
	}
}

func TestNewRejectsABindingCookieThatCollidesWithTheSessionCookie(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Backend = BackendMemory
	cfg.CookieName = testSessionCookie
	if _, err := New(cfg, &stubResolver{}, &fakeSessions{}, testSessionCookie); err == nil {
		t.Fatal("New() accepted the session cookie name as the binding cookie name")
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Backend = "unsupported"
	if _, err := New(cfg, &stubResolver{}, &fakeSessions{}, testSessionCookie); err == nil {
		t.Fatal("New() with an invalid backend = nil error")
	}
}

func TestNewToleratesNilOptions(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Backend = BackendMemory
	p, err := New(cfg, &stubResolver{}, &fakeSessions{}, testSessionCookie, nil, WithClock(nil), WithLogger(nil))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if p.now == nil || p.logger == nil {
		t.Fatal("New() left the clock or logger unset")
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

func federationEnvironment(t *testing.T, plugins map[string]any) *config.Environment {
	t.Helper()
	environment, err := config.NewEnvironment(map[string]any{"plugins": plugins}, "")
	if err != nil {
		t.Fatal(err)
	}
	return environment
}

func constructFederation(t *testing.T, bundles []plugin.Bundle, plugins map[string]any) (*Plugin, error) {
	t.Helper()
	built, err := assembly.BuildPlan(assembly.PlanOptions{Bundles: bundles, Env: federationEnvironment(t, plugins)})
	if err != nil {
		return nil, err
	}
	constructed, err := assembly.Construct(built, assembly.ConstructOptions{})
	if err != nil {
		return nil, err
	}
	instance, found := constructed.Instance(plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
	if !found {
		t.Fatal("the shared base was not constructed")
	}
	p, ok := instance.Primary().(*Plugin)
	if !ok {
		t.Fatalf("primary = %T, want *Plugin", instance.Primary())
	}
	return p, nil
}

func TestAssemblyBindsTheResolverAndSessionAdapter(t *testing.T) {
	p, err := constructFederation(t,
		[]plugin.Bundle{plugin.CombineBundles(plugin.BundleOf(resolverProducer("test-resolver")), Bundle())},
		map[string]any{"session": map[string]any{}, "federation-http": map[string]any{"backend": "memory"}},
	)
	if err != nil {
		t.Fatalf("assembly error = %v", err)
	}
	if _, ok := p.resolver.(*assemblyResolver); !ok {
		t.Fatalf("resolver = %T", p.resolver)
	}
	if _, ok := p.sessions.(*sessionweb.Plugin); !ok {
		t.Fatalf("sessions = %T, want the Web session adapter", p.sessions)
	}
	// The session cookie name comes from plugins.session, not from this
	// module's configuration.
	if p.sessionCookie != sessioncore.DefaultConfig().Name {
		t.Fatalf("session cookie = %q, want %q", p.sessionCookie, sessioncore.DefaultConfig().Name)
	}
}

func TestAssemblyFailsWithoutAnIdentityResolver(t *testing.T) {
	if _, err := constructFederation(t,
		[]plugin.Bundle{Bundle()},
		map[string]any{"session": map[string]any{}, "federation-http": map[string]any{"backend": "memory"}},
	); err == nil {
		t.Fatal("assembly without an IdentityResolver = nil error")
	}
}

func TestAssemblyFailsWithoutASessionAdapter(t *testing.T) {
	if _, err := constructFederation(t,
		[]plugin.Bundle{plugin.CombineBundles(plugin.BundleOf(resolverProducer("test-resolver")), Bundle())},
		map[string]any{"federation-http": map[string]any{"backend": "memory"}},
	); err == nil {
		t.Fatal("assembly without the session plugin = nil error")
	}
}

func TestAssemblyFailsWithTwoIdentityResolvers(t *testing.T) {
	bundles := []plugin.Bundle{plugin.CombineBundles(
		plugin.BundleOf(resolverProducer("test-resolver-one")),
		plugin.BundleOf(resolverProducer("test-resolver-two")),
		Bundle(),
	)}
	if _, err := constructFederation(t, bundles, map[string]any{"session": map[string]any{}, "federation-http": map[string]any{"backend": "memory"}}); err == nil {
		t.Fatal("assembly with two IdentityResolvers = nil error")
	}
}

func TestAssemblyResolvesTheNamedRedisClient(t *testing.T) {
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	redisDefinition := plugin.Define(
		redisPluginKey,
		func(plugin.BuildContext) (*goredis.Client, error) { return client, nil },
		plugin.Options[*goredis.Client]{
			Instances: plugin.MultipleInstances,
			Exports: plugin.Contracts(
				plugin.ExportAs[goredis.UniversalClient](func(value *goredis.Client) goredis.UniversalClient { return value }),
			),
		},
	)

	p, err := constructFederation(t,
		[]plugin.Bundle{plugin.CombineBundles(plugin.BundleOf(redisDefinition), plugin.BundleOf(resolverProducer("test-resolver")), Bundle())},
		map[string]any{
			"redis":           map[string]any{"federation": map[string]any{}},
			"session":         map[string]any{},
			"federation-http": map[string]any{"redis_instance": "federation", "redis_prefix": "test:fed:"},
		},
	)
	if err != nil {
		t.Fatalf("assembly error = %v", err)
	}
	store, ok := p.store.(*RedisStore)
	if !ok {
		t.Fatalf("store = %T, want *RedisStore", p.store)
	}
	if store.client != client {
		t.Fatal("assembly did not wire the named Redis client")
	}
	if store.prefix != "test:fed:" {
		t.Fatalf("prefix = %q", store.prefix)
	}
}

func TestAssemblyDefaultBackendRequiresARedisProducer(t *testing.T) {
	if _, err := constructFederation(t,
		[]plugin.Bundle{plugin.CombineBundles(plugin.BundleOf(resolverProducer("test-resolver")), Bundle())},
		map[string]any{"session": map[string]any{}, "federation-http": map[string]any{}},
	); err == nil {
		t.Fatal("assembly with the default redis backend and no Redis producer = nil error")
	}
}

func TestAssemblyFailsWithoutTheNamedRedisClient(t *testing.T) {
	if _, err := constructFederation(t,
		[]plugin.Bundle{plugin.CombineBundles(plugin.BundleOf(resolverProducer("test-resolver")), Bundle())},
		map[string]any{
			"session":         map[string]any{},
			"federation-http": map[string]any{"backend": "redis", "redis_instance": "federation"},
		},
	); err == nil {
		t.Fatal("assembly with the redis backend and no client = nil error")
	}
}
