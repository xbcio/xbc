package redissyncmap

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/extensions/coordination/syncmap"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// fakeHost is the minimal RuntimeHost a unit test needs. SubmitTask runs the
// function on a goroutine the host cancels and joins at cleanup, so a test
// exercises the same managed-task path production uses.
type fakeHost struct {
	ctx    context.Context
	cancel context.CancelFunc
	log    log.Logger
	refuse bool

	mu sync.Mutex
	wg sync.WaitGroup
}

func newFakeHost() *fakeHost {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeHost{ctx: ctx, cancel: cancel, log: log.Nop()}
}

func (host *fakeHost) ExecutionContext() context.Context { return host.ctx }
func (host *fakeHost) Logger() log.Logger                { return host.log }
func (*fakeHost) ProcessInstance() string                { return "test-process" }
func (*fakeHost) Service() plugin.ServiceIdentity        { return plugin.ServiceIdentity{} }
func (*fakeHost) IngressStopBudget() time.Duration       { return 0 }

func (*fakeHost) TrafficGate() <-chan struct{} {
	gate := make(chan struct{})
	close(gate)
	return gate
}

func (*fakeHost) Admission(plugin.Identity) plugin.Admission                        { return nil }
func (*fakeHost) AdmissionFor(plugin.Identity, plugin.WorkloadKey) plugin.Admission { return nil }
func (*fakeHost) RequestShutdown(plugin.Identity, string) bool                      { return false }

func (host *fakeHost) SubmitTask(_ plugin.Identity, fn func(context.Context), _ bool) bool {
	if host.refuse {
		return false
	}
	host.wg.Add(1)
	go func() {
		defer host.wg.Done()
		fn(host.ctx)
	}()
	return true
}

func (host *fakeHost) join() { host.wg.Wait() }

// testInstance is one factory plus the plugin host that starts it.
type testInstance struct {
	factory *factory
	host    *fakeHost
}

// context builds the lifecycle Context the runtime hands to one stage. It is
// rebuilt per stage rather than retained, which is the contract the plugin
// follows: lifecycle state arrives at the call boundary.
func (instance *testInstance) context() *plugin.Context {
	return plugin.NewRuntimeContext(instance.host, plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
}

// newTestInstance builds a factory over server, initializes it, and registers
// cleanup that stops it before joining the managed tasks.
func newTestInstance(t *testing.T, server *miniredis.Miniredis, config Config) *testInstance {
	t.Helper()

	client := redis.NewClient(&redis.Options{
		Addr:         server.Addr(),
		DialTimeout:  500 * time.Millisecond,
		ReadTimeout:  500 * time.Millisecond,
		WriteTimeout: 500 * time.Millisecond,
		MaxRetries:   -1,
	})
	t.Cleanup(func() { _ = client.Close() })

	created, err := newFactory(log.Nop(), client, config)
	if err != nil {
		t.Fatalf("newFactory: %v", err)
	}
	host := newFakeHost()
	instance := &testInstance{factory: created, host: host}
	if err := initFactory(created, instance.context()); err != nil {
		t.Fatalf("initFactory: %v", err)
	}
	t.Cleanup(func() {
		_ = stopFactory(created, context.Background())
		host.cancel()
		host.join()
	})
	return instance
}

// mapFor creates one map and fails the test if the factory refuses it.
func (instance *testInstance) mapFor(t *testing.T, name string, options ...syncmap.Option) syncmap.Map {
	t.Helper()
	created, err := instance.factory.Map(name, options...)
	if err != nil {
		t.Fatalf("factory.Map(%q): %v", name, err)
	}
	return created
}

// start runs the factory's Start phase.
func (instance *testInstance) start(t *testing.T) {
	t.Helper()
	if err := startFactory(instance.factory, instance.context()); err != nil {
		t.Fatalf("startFactory: %v", err)
	}
}

// waitFor polls condition until it holds or the budget expires.
func waitFor(t *testing.T, budget time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(budget)
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

// eventuallyLoaded waits until the map holds the expected string field of a
// decoded record value.
func eventuallyLoaded(t *testing.T, target syncmap.Map, key, name string) {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool {
		value, ok := target.Load(key)
		if !ok {
			return false
		}
		record, ok := value.(testRecord)
		return ok && record.Name == name
	}, "map did not load key "+key)
}
