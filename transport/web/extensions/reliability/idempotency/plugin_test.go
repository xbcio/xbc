package idempotency

import (
	"reflect"
	"testing"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
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

func TestNewAppliesMemoryDefaultsAndMiddlewareContract(t *testing.T) {
	p, err := New(DefaultConfig())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if p.Order().Phase != web.PhaseBusiness {
		t.Fatalf("Order().Phase = %v, want PhaseBusiness", p.Order().Phase)
	}
	if p.Handler() == nil {
		t.Fatal("Handler() = nil")
	}
	if _, ok := p.state.store.(*MemoryStore); !ok {
		t.Fatalf("memory backend did not construct a MemoryStore, got %T", p.state.store)
	}
}

func TestNewRedisBackendRequiresExplicitStoreForDirectConstruction(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Backend = BackendRedis
	if _, err := New(cfg); err == nil {
		t.Fatal("New() with redis backend and no injected store = nil error")
	}

	injected := NewMemoryStore()
	p, err := New(cfg, WithStore(injected))
	if err != nil {
		t.Fatalf("New() with injected store error = %v", err)
	}
	if p.state.store != injected {
		t.Fatal("injected store was not used")
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Backend = "unsupported"
	if _, err := New(cfg); err == nil {
		t.Fatal("New() with invalid backend = nil error")
	}
}

// TestPlannedDefinitionResolvesNamedRedisClientFromGraph exercises plan(cfg)
// through the real dependency graph: a stub "redis" producer stands in for
// the sibling extensions/storage/redis plugin, proving the redis backend wires the
// exact named *goredis.Client instance the configuration selects.
func TestPlannedDefinitionResolvesNamedRedisClientFromGraph(t *testing.T) {
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	redisDefinition := plugin.Define(
		redisPluginKey,
		func(plugin.BuildContext) (*goredis.Client, error) { return client, nil },
		plugin.Options[*goredis.Client]{Instances: plugin.MultipleInstances},
	)

	environment := idempotencyEnvironment(t, map[string]any{
		"redis": map[string]any{"locks": map[string]any{}},
	}, map[string]any{
		"backend":        "redis",
		"redis_instance": "locks",
		"redis_prefix":   "test:idempotency:",
	})
	built, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{plugin.BundleOf(redisDefinition), Bundle()},
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	constructed, err := assembly.Construct(built, assembly.ConstructOptions{})
	if err != nil {
		t.Fatalf("Construct() error = %v", err)
	}
	instance, found := constructed.Instance(plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
	if !found {
		t.Fatal("idempotency instance not constructed")
	}
	p, ok := instance.Primary().(*Plugin)
	if !ok {
		t.Fatalf("primary = %T, want *Plugin", instance.Primary())
	}
	store, ok := p.state.store.(*RedisStore)
	if !ok {
		t.Fatalf("store = %T, want *RedisStore", p.state.store)
	}
	if store.client != client {
		t.Fatal("plan did not wire the named redis client")
	}
	if store.prefix != "test:idempotency:" {
		t.Fatalf("prefix = %q", store.prefix)
	}
}

// TestPlannedDefinitionRejectsMissingRedisClient proves the redis backend
// fails construction, rather than silently falling back, when the graph has
// no producer for the configured instance.
func TestPlannedDefinitionRejectsMissingRedisClient(t *testing.T) {
	environment := idempotencyEnvironment(t, nil, map[string]any{"backend": "redis"})
	built, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle()},
		Env:     environment,
	})
	if err == nil {
		_, err = assembly.Construct(built, assembly.ConstructOptions{})
	}
	if err == nil {
		t.Fatal("expected an error when the graph has no redis producer")
	}
}

// TestPlannedDefinitionUsesMemoryBackendWithoutAnyInputs proves the memory
// backend never requires a producer plugin at all.
func TestPlannedDefinitionUsesMemoryBackendWithoutAnyInputs(t *testing.T) {
	environment := idempotencyEnvironment(t, nil, map[string]any{})
	built, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle()},
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	constructed, err := assembly.Construct(built, assembly.ConstructOptions{})
	if err != nil {
		t.Fatalf("Construct() error = %v", err)
	}
	instance, found := constructed.Instance(plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
	if !found {
		t.Fatal("idempotency instance not constructed")
	}
	p := instance.Primary().(*Plugin)
	if _, ok := p.state.store.(*MemoryStore); !ok {
		t.Fatalf("store = %T, want *MemoryStore", p.state.store)
	}
}

func idempotencyEnvironment(t *testing.T, otherPlugins map[string]any, idempotency map[string]any) *config.Environment {
	t.Helper()
	plugins := map[string]any{"idempotency": idempotency}
	for key, value := range otherPlugins {
		plugins[key] = value
	}
	environment, err := config.NewEnvironment(map[string]any{"plugins": plugins}, "")
	if err != nil {
		t.Fatal(err)
	}
	return environment
}
