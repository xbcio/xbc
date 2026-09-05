package session

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/internal/assembly"
	"github.com/xbcio/xbc/plugin"
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
	t.Cleanup(func() { _ = p.Stop(context.Background()) })

	order := p.Order()
	if order.Phase != web.PhaseAuth {
		t.Fatalf("Order().Phase = %v, want PhaseAuth", order.Phase)
	}
	if len(order.Before) != 2 || order.Before[0] != web.Prefer(tenantKey) || order.Before[1] != web.Prefer(casbinKey) {
		t.Fatalf("Order().Before = %#v", order.Before)
	}
	if p.Handler() == nil {
		t.Fatal("Handler() = nil")
	}
	if _, ok := p.manager.store.(*MemoryStore); !ok {
		t.Fatalf("memory backend did not construct a MemoryStore, got %T", p.manager.store)
	}
}

func TestNewRedisBackendRequiresExplicitStoreForDirectConstruction(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Backend = BackendRedis
	if _, err := New(cfg); err == nil {
		t.Fatal("New() with redis backend and no injected store = nil error")
	}

	injected := &recordingStore{}
	p, err := New(cfg, WithStore(injected))
	if err != nil {
		t.Fatalf("New() with injected store error = %v", err)
	}
	if p.manager.store != injected {
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
// the sibling integrations/redis plugin, proving the redis backend wires the
// exact named *goredis.Client instance the configuration selects and never
// closes that borrowed client on Stop.
func TestPlannedDefinitionResolvesNamedRedisClientFromGraph(t *testing.T) {
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	redisDefinition := plugin.Define(
		redisPluginKey,
		func(plugin.BuildContext) (*goredis.Client, error) { return client, nil },
		plugin.Options[*goredis.Client]{Instances: plugin.MultipleInstances},
	)

	environment := sessionEnvironment(t, map[string]any{
		"redis": map[string]any{"sessions": map[string]any{}},
	}, map[string]any{
		"backend":        "redis",
		"redis_instance": "sessions",
		"redis_prefix":   "test:session:",
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
		t.Fatal("session instance not constructed")
	}
	p, ok := instance.Primary().(*Plugin)
	if !ok {
		t.Fatalf("primary = %T, want *Plugin", instance.Primary())
	}
	store, ok := p.manager.store.(*RedisStore)
	if !ok {
		t.Fatalf("store = %T, want *RedisStore", p.manager.store)
	}
	if store.client != client {
		t.Fatal("plan did not wire the named redis client")
	}
	if store.prefix != "test:session:" {
		t.Fatalf("prefix = %q", store.prefix)
	}

	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Stop() closed the borrowed Redis client: %v", err)
	}
}

// TestPlannedDefinitionRejectsMissingRedisClient proves the redis backend
// fails construction, rather than silently falling back, when the graph has
// no producer for the configured instance.
func TestPlannedDefinitionRejectsMissingRedisClient(t *testing.T) {
	environment := sessionEnvironment(t, nil, map[string]any{"backend": "redis"})
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
	environment := sessionEnvironment(t, nil, map[string]any{})
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
		t.Fatal("session instance not constructed")
	}
	p := instance.Primary().(*Plugin)
	if _, ok := p.manager.store.(*MemoryStore); !ok {
		t.Fatalf("store = %T, want *MemoryStore", p.manager.store)
	}
	_ = p.Stop(context.Background())
}

func TestStopIsIdempotentAndDisablesTheManager(t *testing.T) {
	p, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	manager := p.Manager()

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 32 {
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

	if _, err := manager.Create(context.Background(), "alice", nil); !errors.Is(err, ErrStopped) {
		t.Fatalf("Create after Stop error = %v", err)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
}

func TestInjectedStoreBypassesRedisDependencyAndRemainsCallerOwned(t *testing.T) {
	var deletes int
	store := &recordingStore{delete: func(context.Context, string) error {
		deletes++
		return nil
	}}
	cfg := DefaultConfig()
	cfg.Backend = BackendRedis
	cfg.RedisInstance = "unavailable"
	p, err := New(cfg, WithStore(store))
	if err != nil {
		t.Fatal(err)
	}

	manager := p.Manager()
	if err := manager.Revoke(context.Background(), testID(80, 32)); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deletes != 1 {
		t.Fatalf("injected store calls = %d", deletes)
	}
}
