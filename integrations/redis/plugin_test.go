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

	"github.com/xbcio/xbc/plugin"
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

func TestNewClientPingsAndMapsConfig(t *testing.T) {
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

	client, err := newClient(plugin.BuildContext{}, cfg)
	if err != nil {
		t.Fatalf("newClient() error = %v", err)
	}
	t.Cleanup(func() { _ = stopClient(client, context.Background()) })
	if server.CommandCount() == 0 {
		t.Fatal("newClient() sent no Redis command; default ping did not validate the connection")
	}

	options := client.Options()
	if options.Addr != cfg.Addr || options.Username != cfg.Username || options.Password != cfg.Password || options.DB != cfg.DB {
		t.Fatalf("client identity options = %#v, want config values", options)
	}
	if options.DialTimeout != cfg.DialTimeout || options.ReadTimeout != cfg.ReadTimeout || options.WriteTimeout != cfg.WriteTimeout || options.PoolTimeout != cfg.PoolTimeout {
		t.Fatalf("client timeout options = %#v, want config values", options)
	}
	if options.PoolSize != cfg.PoolSize || options.MinIdleConns != cfg.MinIdleConns || options.MaxIdleConns != cfg.MaxIdleConns || options.MaxActiveConns != cfg.MaxActiveConns {
		t.Fatalf("client pool options = %#v, want config values", options)
	}

	if err := client.Set(context.Background(), "instance", "cache", 0).Err(); err != nil {
		t.Fatalf("client Set() error = %v", err)
	}
	value, err := server.DB(4).Get("instance")
	if err != nil || value != "cache" {
		t.Fatalf("selected DB value = %q, %v; want cache in DB 4", value, err)
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

	// This client has been constructed but has not opened a connection. That is
	// the earliest state at which ownership can transfer to the lifecycle.
	client := goredis.NewClient((&Config{Addr: "127.0.0.1:1"}).options())

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
	if err := client.Ping(context.Background()).Err(); !errors.Is(err, goredis.ErrClosed) {
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
