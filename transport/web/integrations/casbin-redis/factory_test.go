package casbinredis

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	casbinlib "github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"
	"github.com/casbin/casbin/v2/persist"
	rediswatcher "github.com/casbin/redis-watcher/v2"
	goredis "github.com/redis/go-redis/v9"
)

const testModel = `
[request_definition]
r = sub, obj, act

[policy_definition]
p = sub, obj, act

[role_definition]
g = _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = g(r.sub, p.sub) && r.obj == p.obj && r.act == p.act
`

func TestOpenRejectsNilFactoryAndEnforcer(t *testing.T) {
	enforcer := newTestEnforcer(t)
	var nilFactory *Factory
	if watcher, err := nilFactory.Open(enforcer); err == nil || watcher != nil {
		t.Fatalf("nil Factory Open() = (%T, %v)", watcher, err)
	}

	factory, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if watcher, err := factory.Open(nil); err == nil || watcher != nil {
		t.Fatalf("nil enforcer Open() = (%T, %v)", watcher, err)
	}
}

func TestStandaloneOpenPropagatesOptionsAndCleansUpConstructorFailure(t *testing.T) {
	server := miniredis.RunT(t)
	server.RequireUserAuth("service", "correct-password")

	cfg := DefaultConfig()
	cfg.Addrs = []string{server.Addr()}
	cfg.Username = "service"
	cfg.Password = "correct-password"
	cfg.DB = 4
	cfg.DialTimeout = 700 * time.Millisecond
	cfg.ReadTimeout = 800 * time.Millisecond
	cfg.WriteTimeout = 900 * time.Millisecond
	cfg.Channel = "/authorization"
	cfg.IgnoreSelf = false

	var subscriber, publisher *goredis.Client
	constructorErr := errors.New("constructor failed after opening clients")
	factory, err := newFactory(cfg, watcherConstructors{
		standalone: func(addr string, options rediswatcher.WatcherOptions) (persist.Watcher, error) {
			if addr != server.Addr() {
				t.Fatalf("constructor addr = %q, want %q", addr, server.Addr())
			}
			if options.Options.Username != cfg.Username || options.Options.Password != cfg.Password || options.Options.DB != cfg.DB {
				t.Fatalf("identity options = %#v", options.Options)
			}
			if options.Options.DialTimeout != cfg.DialTimeout || options.Options.ReadTimeout != cfg.ReadTimeout || options.Options.WriteTimeout != cfg.WriteTimeout {
				t.Fatalf("timeout options = %#v", options.Options)
			}
			if options.Channel != cfg.Channel || options.IgnoreSelf != cfg.IgnoreSelf || options.OptionalUpdateCallback == nil {
				t.Fatalf("watcher options = %#v", options)
			}
			subscriber, publisher = options.SubClient, options.PubClient
			if subscriber == nil || publisher == nil || subscriber == publisher {
				t.Fatalf("watcher clients = (%p, %p), want distinct non-nil clients", subscriber, publisher)
			}
			if err := subscriber.Ping(context.Background()).Err(); err != nil {
				t.Fatalf("subscriber Ping() error = %v", err)
			}
			if err := publisher.Ping(context.Background()).Err(); err != nil {
				t.Fatalf("publisher Ping() error = %v", err)
			}
			return nil, constructorErr
		},
	})
	if err != nil {
		t.Fatalf("newFactory() error = %v", err)
	}

	watcher, err := factory.Open(newTestEnforcer(t))
	if watcher != nil || !errors.Is(err, constructorErr) {
		t.Fatalf("Open() = (%T, %v), want constructor error", watcher, err)
	}
	if err := subscriber.Ping(context.Background()).Err(); !errors.Is(err, goredis.ErrClosed) {
		t.Fatalf("subscriber after failure error = %v, want redis.ErrClosed", err)
	}
	if err := publisher.Ping(context.Background()).Err(); !errors.Is(err, goredis.ErrClosed) {
		t.Fatalf("publisher after failure error = %v, want redis.ErrClosed", err)
	}
	await(t, "failed constructor connections to close", func() bool { return server.CurrentConnectionCount() == 0 })
}

func TestOpenConnectionFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("listener.Close() error = %v", err)
	}

	cfg := DefaultConfig()
	cfg.Addrs = []string{addr}
	cfg.DialTimeout = 50 * time.Millisecond
	factory, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	watcher, err := factory.Open(newTestEnforcer(t))
	if err == nil || watcher != nil {
		t.Fatalf("Open() = (%T, %v), want connection failure", watcher, err)
	}
	if !strings.Contains(err.Error(), addr) {
		t.Fatalf("Open() error = %v, want failed address", err)
	}
}

func TestOpenAuthenticationFailureDoesNotLeakConnectionsOrPassword(t *testing.T) {
	server := miniredis.RunT(t)
	server.RequireAuth("correct-password")
	cfg := DefaultConfig()
	cfg.Addrs = []string{server.Addr()}
	cfg.Password = "wrong-password-that-must-stay-secret"
	cfg.DialTimeout = 100 * time.Millisecond

	factory, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	watcher, err := factory.Open(newTestEnforcer(t))
	if err == nil || watcher != nil {
		t.Fatalf("Open() = (%T, %v), want authentication failure", watcher, err)
	}
	if strings.Contains(err.Error(), cfg.Password) {
		t.Fatalf("Open() leaked password: %v", err)
	}
	await(t, "authentication-failure connection cleanup", func() bool { return server.CurrentConnectionCount() == 0 })
}

func TestClusterOpenUsesClusterConstructorAndConfiguredOptions(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = ModeCluster
	cfg.Addrs = []string{"redis-a.internal:6379", "redis-b.internal:6380"}
	cfg.Username = "service"
	cfg.Password = "secret"
	cfg.DialTimeout = 2 * time.Second
	cfg.ReadTimeout = 4 * time.Second
	cfg.WriteTimeout = 6 * time.Second
	cfg.Channel = "/cluster-casbin"

	returned := new(stubWatcher)
	var standaloneCalls, clusterCalls atomic.Int32
	factory, err := newFactory(cfg, watcherConstructors{
		standalone: func(string, rediswatcher.WatcherOptions) (persist.Watcher, error) {
			standaloneCalls.Add(1)
			return nil, errors.New("standalone constructor must not run")
		},
		cluster: func(addrs string, options rediswatcher.WatcherOptions) (persist.Watcher, error) {
			clusterCalls.Add(1)
			if addrs != strings.Join(cfg.Addrs, ",") {
				t.Fatalf("cluster addrs = %q", addrs)
			}
			got := options.ClusterOptions
			if strings.Join(got.Addrs, ",") != strings.Join(cfg.Addrs, ",") || got.Username != cfg.Username || got.Password != cfg.Password {
				t.Fatalf("cluster identity options = %#v", got)
			}
			if got.DialTimeout != cfg.DialTimeout || got.ReadTimeout != cfg.ReadTimeout || got.WriteTimeout != cfg.WriteTimeout || got.Dialer == nil {
				t.Fatalf("cluster connection options = %#v", got)
			}
			if options.Channel != cfg.Channel || !options.IgnoreSelf || options.OptionalUpdateCallback == nil {
				t.Fatalf("watcher options = %#v", options)
			}
			return returned, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	watcher, err := factory.Open(newTestEnforcer(t))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if watcher != returned || clusterCalls.Load() != 1 || standaloneCalls.Load() != 0 {
		t.Fatalf("Open() watcher/calls = (%T, %d, %d)", watcher, clusterCalls.Load(), standaloneCalls.Load())
	}
	watcher.Close()
	if returned.closeCalls.Load() != 1 {
		t.Fatalf("watcher Close calls = %d, want 1", returned.closeCalls.Load())
	}
}

func TestClusterOpenUsesCredentialsAndOwnsItsConnections(t *testing.T) {
	server := miniredis.RunT(t)
	server.RequireUserAuth("casbin-service", "cluster-secret")
	cfg := DefaultConfig()
	cfg.Mode = ModeCluster
	cfg.Addrs = []string{server.Addr()}
	cfg.Username = "casbin-service"
	cfg.Password = "cluster-secret"
	cfg.Channel = "/cluster-ownership"

	factory, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	watcher, err := factory.Open(newTestEnforcer(t))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	await(t, "cluster watcher connections", func() bool {
		return server.CurrentConnectionCount() > 0 && server.PubSubNumSub(cfg.Channel)[cfg.Channel] == 1
	})
	watcher.Close()
	await(t, "cluster watcher-owned connections to close", func() bool {
		return server.CurrentConnectionCount() == 0
	})
}

func TestClusterConstructorFailureClosesTrackedConnections(t *testing.T) {
	server := miniredis.RunT(t)
	cfg := DefaultConfig()
	cfg.Mode = ModeCluster
	cfg.Addrs = []string{server.Addr()}
	constructorErr := errors.New("cluster constructor failed")

	factory, err := newFactory(cfg, watcherConstructors{
		cluster: func(_ string, options rediswatcher.WatcherOptions) (persist.Watcher, error) {
			for i := 0; i < 2; i++ {
				connection, err := options.ClusterOptions.Dialer(context.Background(), "tcp", server.Addr())
				if err != nil {
					t.Fatalf("tracked Dialer() error = %v", err)
				}
				if connection == nil {
					t.Fatal("tracked Dialer() returned nil connection")
				}
			}
			await(t, "cluster constructor connections", func() bool {
				return server.CurrentConnectionCount() == 2
			})
			return nil, constructorErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	watcher, err := factory.Open(newTestEnforcer(t))
	if watcher != nil || !errors.Is(err, constructorErr) {
		t.Fatalf("Open() = (%T, %v), want constructor failure", watcher, err)
	}
	await(t, "cluster construction connections to close", func() bool { return server.CurrentConnectionCount() == 0 })
}

func TestTwoWatchersSynchronizePolicyGroupingAndFilteredRemoval(t *testing.T) {
	server := miniredis.RunT(t)
	cfg := DefaultConfig()
	cfg.Addrs = []string{server.Addr()}
	cfg.Channel = "/casbin-sync-test"
	factory, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	first := newTestEnforcer(t)
	second := newTestEnforcer(t)
	firstWatcher, err := factory.Open(first)
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	defer firstWatcher.Close()
	secondWatcher, err := factory.Open(second)
	if err != nil {
		t.Fatalf("Open(second) error = %v", err)
	}
	defer secondWatcher.Close()
	if firstWatcher == secondWatcher {
		t.Fatal("Open returned the same watcher for two enforcers")
	}
	if err := first.SetWatcher(firstWatcher); err != nil {
		t.Fatalf("first.SetWatcher() error = %v", err)
	}
	if err := second.SetWatcher(secondWatcher); err != nil {
		t.Fatalf("second.SetWatcher() error = %v", err)
	}
	await(t, "both Redis subscriptions", func() bool {
		return server.PubSubNumSub(cfg.Channel)[cfg.Channel] == 2
	})

	added, err := first.AddPolicy("admin", "orders", "read")
	if err != nil || !added {
		t.Fatalf("AddPolicy() = (%v, %v)", added, err)
	}
	await(t, "policy synchronization", func() bool {
		has, err := second.HasPolicy("admin", "orders", "read")
		return err == nil && has
	})

	added, err = first.AddGroupingPolicy("alice", "admin")
	if err != nil || !added {
		t.Fatalf("AddGroupingPolicy() = (%v, %v)", added, err)
	}
	await(t, "grouping synchronization", func() bool {
		has, err := second.HasGroupingPolicy("alice", "admin")
		return err == nil && has
	})
	await(t, "RBAC enforcement synchronization", func() bool {
		allowed, err := second.Enforce("alice", "orders", "read")
		return err == nil && allowed
	})

	removed, err := first.RemoveFilteredPolicy(0, "admin")
	if err != nil || !removed {
		t.Fatalf("RemoveFilteredPolicy() = (%v, %v)", removed, err)
	}
	await(t, "filtered policy removal synchronization", func() bool {
		has, err := second.HasPolicy("admin", "orders", "read")
		return err == nil && !has
	})

	removed, err = first.RemoveFilteredGroupingPolicy(0, "alice")
	if err != nil || !removed {
		t.Fatalf("RemoveFilteredGroupingPolicy() = (%v, %v)", removed, err)
	}
	await(t, "filtered grouping removal synchronization", func() bool {
		has, err := second.HasGroupingPolicy("alice", "admin")
		return err == nil && !has
	})
}

func TestConnectionTrackerRejectsDialAfterFailure(t *testing.T) {
	server := miniredis.RunT(t)
	tracker := newConnectionTracker(time.Second)
	connection, err := tracker.DialContext(context.Background(), "tcp", server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.closeConstructionConnections(); err != nil {
		t.Fatalf("closeConstructionConnections() error = %v", err)
	}
	if _, err := tracker.DialContext(context.Background(), "tcp", server.Addr()); err == nil {
		t.Fatal("DialContext() after failure succeeded")
	}
	if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("repeated tracked connection Close() error = %v", err)
	}
	await(t, "tracker connections to close", func() bool { return server.CurrentConnectionCount() == 0 })
}

func newTestEnforcer(t *testing.T) *casbinlib.SyncedEnforcer {
	t.Helper()
	m, err := model.NewModelFromString(testModel)
	if err != nil {
		t.Fatalf("NewModelFromString() error = %v", err)
	}
	enforcer, err := casbinlib.NewSyncedEnforcer(m)
	if err != nil {
		t.Fatalf("NewSyncedEnforcer() error = %v", err)
	}
	return enforcer
}

func await(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

type stubWatcher struct {
	callback   func(string)
	updates    atomic.Int32
	closeCalls atomic.Int32
}

func (w *stubWatcher) SetUpdateCallback(callback func(string)) error {
	w.callback = callback
	return nil
}
func (w *stubWatcher) Update() error {
	w.updates.Add(1)
	return nil
}
func (w *stubWatcher) Close() { w.closeCalls.Add(1) }

func (w *stubWatcher) String() string {
	return fmt.Sprintf("stubWatcher(updates=%d)", w.updates.Load())
}
