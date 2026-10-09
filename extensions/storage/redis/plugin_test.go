package redis

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	xbcconfig "github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

func TestDefinitionIsCanonicalAndBundleIsStable(t *testing.T) {
	var zeroDefinition plugin.Definition
	if Definition() == zeroDefinition {
		t.Fatal("Definition() returned a zero handle")
	}
	if Definition() != Definition() {
		t.Fatal("Definition() returned different handles")
	}

	first := Bundle()
	second := Bundle()
	if reflect.DeepEqual(first, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("Bundle() returned different composition content")
	}
}

// TestAssembledPluginExportsTheTopologyNeutralContract resolves the exported
// contract through the real graph. Consumers outside this module (session,
// idempotency, cron) cannot import it, so they resolve goredis.UniversalClient
// by identity; a Definition that stopped exporting it would fail at plan time
// in every application composing them, a failure no test outside this module
// can observe.
func TestAssembledPluginExportsTheTopologyNeutralContract(t *testing.T) {
	server := miniredis.RunT(t)

	environment, err := xbcconfig.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"redis": map[string]any{
				"cache": map[string]any{"addr": server.Addr()},
			},
		},
	}, "XBC_REDIS_CONTRACT_TEST_UNSET_")
	if err != nil {
		t.Fatalf("config.NewEnvironment: %v", err)
	}

	contractInput := plugin.RefToInstance[goredis.UniversalClient](Key, "cache")
	consumer := plugin.Define(
		"cache-consumer",
		func(ctx plugin.BuildContext) (*contractConsumer, error) {
			return &contractConsumer{client: contractInput.Get(ctx).Value}, nil
		},
		plugin.Options[*contractConsumer]{Inputs: plugin.Inputs(contractInput)},
	)

	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{plugin.BundleOf(consumer), Bundle()},
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("assembly.BuildPlan: %v", err)
	}
	constructed, err := assembly.Construct(plan, assembly.ConstructOptions{})
	if err != nil {
		t.Fatalf("assembly.Construct: %v", err)
	}
	t.Cleanup(func() {
		if instance, ok := constructed.Instance(plugin.Identity{Plugin: Key, Instance: "cache"}); ok {
			_ = instance.StopBounded(context.Background(), time.Second)
		}
	})

	instance, ok := constructed.Instance(plugin.Identity{Plugin: Key, Instance: "cache"})
	if !ok {
		t.Fatal("the configured redis instance must be constructed")
	}
	primary, ok := instance.Primary().(*Client)
	if !ok {
		t.Fatalf("primary = %T, want *Client", instance.Primary())
	}
	consumed, ok := constructed.Instance(plugin.Identity{Plugin: "cache-consumer", Instance: plugin.DefaultInstance})
	if !ok {
		t.Fatal("the consumer must be constructed")
	}
	probe, ok := consumed.Primary().(*contractConsumer)
	if !ok {
		t.Fatalf("consumer primary = %T, want *contractConsumer", consumed.Primary())
	}
	if probe.client != goredis.UniversalClient(primary) {
		t.Fatal("the consumer resolved a different client than the instance's primary")
	}
}

type contractConsumer struct {
	client goredis.UniversalClient
}

func TestBuildClientMapsStandaloneConfigAndPings(t *testing.T) {
	server := miniredis.RunT(t)
	server.RequireUserAuth("service", "secret")

	cfg := bindConfig(t, map[string]any{
		"addr":               server.Addr(),
		"username":           "service",
		"password":           "secret",
		"db":                 4,
		"dial_timeout":       "250ms",
		"read_timeout":       "300ms",
		"write_timeout":      "350ms",
		"pool_timeout":       "400ms",
		"pool_size":          4,
		"min_idle_conns":     0,
		"max_idle_conns":     2,
		"max_active_conns":   6,
		"conn_max_idle_time": "2m",
		"conn_max_lifetime":  "5m",
		"max_retries":        1,
	})

	// The recording factory builds the real client from the options the plugin
	// chose, so the assertions below describe the connection it would open. The
	// wrapper hides Options(), which is why they read the request instead of the
	// client.
	factory := defaultClientFactory()
	var opened *goredis.Options
	factory.standalone = func(options *goredis.Options) goredis.UniversalClient {
		opened = options
		return goredis.NewClient(options)
	}

	client, err := buildClient(factory, cfg)
	if err != nil {
		t.Fatalf("buildClient() error = %v", err)
	}
	t.Cleanup(func() { _ = stopClient(client, context.Background()) })
	if opened == nil {
		t.Fatal("buildClient() did not use the configured factory")
	}
	if server.CommandCount() == 0 {
		t.Fatal("buildClient() sent no Redis command; default ping did not validate the connection")
	}

	if opened.Addr != cfg.Addr || opened.Username != cfg.Username || opened.Password != cfg.Password || opened.DB != cfg.DB {
		t.Fatalf("client identity options = %#v, want config values", opened)
	}
	if opened.DialTimeout != cfg.DialTimeout || opened.ReadTimeout != cfg.ReadTimeout || opened.WriteTimeout != cfg.WriteTimeout || opened.PoolTimeout != cfg.PoolTimeout {
		t.Fatalf("client timeout options = %#v, want config values", opened)
	}
	if opened.PoolSize != cfg.PoolSize || opened.MinIdleConns != cfg.MinIdleConns || opened.MaxIdleConns != cfg.MaxIdleConns || opened.MaxActiveConns != cfg.MaxActiveConns {
		t.Fatalf("client pool options = %#v, want config values", opened)
	}
	if opened.ConnMaxIdleTime != cfg.ConnMaxIdleTime || opened.ConnMaxLifetime != cfg.ConnMaxLifetime || opened.MaxRetries != cfg.MaxRetries {
		t.Fatalf("client connection options = %#v, want config values", opened)
	}

	if err := client.Set(context.Background(), "instance", "cache", 0).Err(); err != nil {
		t.Fatalf("client Set() error = %v", err)
	}
	value, err := server.DB(4).Get("instance")
	if err != nil || value != "cache" {
		t.Fatalf("selected DB value = %q, %v; want cache in DB 4", value, err)
	}
}

// TestBuildClientSelectsTheConstructorItsModeNames covers the routing a
// sentinel deployment depends on: go-redis panics when RouteByLatency or
// RouteRandomly reach NewFailoverClient, so either flag must select the
// sentinel-backed cluster client instead. Every case records which constructor
// the build path called; ping is off, so each factory may hand back an
// unconnected client.
func TestBuildClientSelectsTheConstructorItsModeNames(t *testing.T) {
	tests := []struct {
		name     string
		values   map[string]any
		selected string
	}{
		{name: "sentinel", selected: "failover", values: map[string]any{
			"mode": "sentinel", "addrs": []any{"10.0.0.1:26379"}, "master_name": "primary", "ping": false,
		}},
		{name: "sentinel routing by latency", selected: "failoverCluster", values: map[string]any{
			"mode": "sentinel", "addrs": []any{"10.0.0.1:26379"}, "master_name": "primary", "route_by_latency": true, "ping": false,
		}},
		{name: "sentinel routing randomly", selected: "failoverCluster", values: map[string]any{
			"mode": "sentinel", "addrs": []any{"10.0.0.1:26379"}, "master_name": "primary", "route_randomly": true, "ping": false,
		}},
		{name: "cluster", selected: "cluster", values: map[string]any{
			"mode": "cluster", "addrs": []any{"10.0.0.1:6379"}, "ping": false,
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := preparedConfig(t, test.values)

			var calls []string
			client, err := buildClient(recordingFactory(&calls), cfg)
			if err != nil {
				t.Fatalf("buildClient() error = %v", err)
			}
			t.Cleanup(func() { _ = stopClient(client, context.Background()) })

			if len(calls) != 1 || calls[0] != test.selected {
				t.Fatalf("buildClient() called %v, want exactly [%s]", calls, test.selected)
			}
		})
	}
}

// recordingFactory returns a factory that records which constructor a build
// selected and hands back an unconnected client, so the routing of a mode is
// observable without a live topology of that mode.
func recordingFactory(calls *[]string) clientFactory {
	inert := func() goredis.UniversalClient {
		return goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
	}
	record := func(name string) {
		*calls = append(*calls, name)
	}
	return clientFactory{
		standalone: func(*goredis.Options) goredis.UniversalClient {
			record("standalone")
			return inert()
		},
		failover: func(*goredis.FailoverOptions) goredis.UniversalClient {
			record("failover")
			return inert()
		},
		failoverCluster: func(*goredis.FailoverOptions) goredis.UniversalClient {
			record("failoverCluster")
			return inert()
		},
		cluster: func(*goredis.ClusterOptions) goredis.UniversalClient {
			record("cluster")
			return inert()
		},
	}
}

func TestBuildClientMapsSentinelConfig(t *testing.T) {
	cfg := preparedConfig(t, map[string]any{
		"mode":              "sentinel",
		"addrs":             []any{"10.0.0.1:26379", "10.0.0.2:26379"},
		"master_name":       "primary",
		"sentinel_username": "watcher",
		"sentinel_password": "sentinel-secret",
		"username":          "service",
		"password":          "secret",
		"db":                3,
		"dial_timeout":      "250ms",
		"pool_size":         4,
		"max_retries":       1,
		"ping":              false,
	})

	factory := defaultClientFactory()
	var opened *goredis.FailoverOptions
	factory.failover = func(options *goredis.FailoverOptions) goredis.UniversalClient {
		opened = options
		return goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
	}

	client, err := buildClient(factory, cfg)
	if err != nil {
		t.Fatalf("buildClient() error = %v", err)
	}
	t.Cleanup(func() { _ = stopClient(client, context.Background()) })
	if opened == nil {
		t.Fatal("buildClient() did not use the sentinel constructor")
	}

	if opened.MasterName != cfg.MasterName || !reflect.DeepEqual(opened.SentinelAddrs, cfg.Addrs) {
		t.Fatalf("failover topology options = %#v, want master %q at %v", opened, cfg.MasterName, cfg.Addrs)
	}
	if opened.SentinelUsername != cfg.SentinelUsername || opened.SentinelPassword != cfg.SentinelPassword {
		t.Fatalf("failover sentinel credentials = %#v, want the configured ones", opened)
	}
	if opened.Username != cfg.Username || opened.Password != cfg.Password || opened.DB != cfg.DB {
		t.Fatalf("failover identity options = %#v, want config values", opened)
	}
	if opened.DialTimeout != cfg.DialTimeout || opened.PoolSize != cfg.PoolSize || opened.MaxRetries != cfg.MaxRetries {
		t.Fatalf("failover connection options = %#v, want config values", opened)
	}
	if opened.RouteByLatency || opened.RouteRandomly {
		t.Fatalf("failover route flags = %#v, want the un-routed constructor to receive none", opened)
	}
}

func TestBuildClientMapsClusterConfig(t *testing.T) {
	cfg := preparedConfig(t, map[string]any{
		"mode":          "cluster",
		"addrs":         []any{"10.0.0.1:6379", "10.0.0.2:6379"},
		"username":      "service",
		"password":      "secret",
		"read_only":     true,
		"max_redirects": 5,
		"pool_size":     4,
		"max_retries":   2,
		"ping":          false,
	})

	factory := defaultClientFactory()
	var opened *goredis.ClusterOptions
	factory.cluster = func(options *goredis.ClusterOptions) goredis.UniversalClient {
		opened = options
		return goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
	}

	client, err := buildClient(factory, cfg)
	if err != nil {
		t.Fatalf("buildClient() error = %v", err)
	}
	t.Cleanup(func() { _ = stopClient(client, context.Background()) })
	if opened == nil {
		t.Fatal("buildClient() did not use the cluster constructor")
	}

	if !reflect.DeepEqual(opened.Addrs, cfg.Addrs) {
		t.Fatalf("cluster seeds = %v, want %v", opened.Addrs, cfg.Addrs)
	}
	if opened.Username != cfg.Username || opened.Password != cfg.Password {
		t.Fatalf("cluster identity options = %#v, want config values", opened)
	}
	if opened.ReadOnly != cfg.ReadOnly || opened.MaxRedirects != cfg.MaxRedirects {
		t.Fatalf("cluster routing options = %#v, want read_only %t and max_redirects %d", opened, cfg.ReadOnly, cfg.MaxRedirects)
	}
	if opened.PoolSize != cfg.PoolSize {
		t.Fatalf("cluster pool options = %#v, want config values", opened)
	}
	// The configured max_retries reaches the cluster client too: the framework's
	// field means the same thing in every mode, so it is migrated rather than
	// silently dropped in favour of the cluster client's own default.
	if opened.MaxRetries != cfg.MaxRetries {
		t.Fatalf("cluster MaxRetries = %d, want the configured %d", opened.MaxRetries, cfg.MaxRetries)
	}
}

func TestClusterClientPingsTheConfiguredSeeds(t *testing.T) {
	server := miniredis.RunT(t)

	cfg := preparedConfig(t, map[string]any{
		"mode":  "cluster",
		"addrs": []any{server.Addr()},
	})

	client, err := newClient(plugin.BuildContext{}, cfg)
	if err != nil {
		t.Fatalf("newClient() error = %v", err)
	}
	t.Cleanup(func() { _ = stopClient(client, context.Background()) })
	if server.CommandCount() == 0 {
		t.Fatal("newClient() sent no Redis command; default ping did not validate the connection")
	}
}

func TestSentinelPingFailureNamesTheTopology(t *testing.T) {
	server := miniredis.RunT(t)
	sentinelAddr := server.Addr()
	server.Close()

	cfg := preparedConfig(t, map[string]any{
		"mode":          "sentinel",
		"addrs":         []any{sentinelAddr},
		"master_name":   "primary",
		"dial_timeout":  "100ms",
		"read_timeout":  "100ms",
		"write_timeout": "100ms",
		"pool_timeout":  "100ms",
	})

	client, err := newClient(plugin.BuildContext{}, cfg)
	if err == nil || !strings.Contains(err.Error(), `redis: ping sentinel master "primary" at `+sentinelAddr) {
		t.Fatalf("newClient() error = %v, want the sentinel topology named", err)
	}
	if client != nil {
		t.Fatal("newClient() returned a client after ping failure")
	}
}

func TestNewClientCanDisablePing(t *testing.T) {
	cfg := bindConfig(t, map[string]any{
		"addr":         "127.0.0.1:1",
		"dial_timeout": "10ms",
		"ping":         false,
	})

	client, err := newClient(plugin.BuildContext{}, cfg)
	if err != nil {
		t.Fatalf("newClient() with ping=false error = %v", err)
	}
	if client == nil {
		t.Fatal("newClient() with ping=false returned a nil client")
	}
	if err := stopClient(client, context.Background()); err != nil {
		t.Fatalf("stopClient() error = %v", err)
	}
}

func TestNewClientPingFailureClosesLocally(t *testing.T) {
	server := miniredis.RunT(t)
	server.SetError("LOADING test failure")

	cfg := bindConfig(t, map[string]any{
		"addr":          server.Addr(),
		"max_retries":   -1,
		"dial_timeout":  "100ms",
		"read_timeout":  "100ms",
		"write_timeout": "100ms",
		"pool_timeout":  "100ms",
	})

	client, err := newClient(plugin.BuildContext{}, cfg)
	if err == nil || !strings.Contains(err.Error(), "redis: ping ") {
		t.Fatalf("newClient() error = %v, want contextual ping failure", err)
	}
	if client != nil {
		t.Fatal("newClient() returned a client after ping failure")
	}
	waitFor(t, time.Second, func() bool { return server.CurrentConnectionCount() == 0 }, "failed factory left a Redis connection open")
}

func TestStopClientIsSafeForPartialStateAndConcurrentCalls(t *testing.T) {
	if err := stopClient(nil, nil); err != nil {
		t.Fatalf("stopClient(nil) error = %v", err)
	}
	if err := stopClient(&Client{}, nil); err != nil {
		t.Fatalf("stopClient(empty wrapper) error = %v", err)
	}

	// This client has been constructed but has not opened a connection. That is
	// the earliest state at which ownership can transfer to the lifecycle.
	raw := goredis.NewClient((&Config{Addr: "127.0.0.1:1"}).options())
	client := wrapClient(raw)

	const closers = 16
	errs := make(chan error, closers)
	var wg sync.WaitGroup
	wg.Add(closers)
	for range closers {
		go func() {
			defer wg.Done()
			errs <- stopClient(client, context.Background())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent stopClient() error = %v", err)
		}
	}
	if err := stopClient(client, context.Background()); err != nil {
		t.Fatalf("repeated stopClient() error = %v", err)
	}
	if err := raw.Ping(context.Background()).Err(); !errors.Is(err, goredis.ErrClosed) {
		t.Fatalf("Ping() after stop error = %v, want redis.ErrClosed", err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !condition() {
		t.Fatal(message)
	}
}
