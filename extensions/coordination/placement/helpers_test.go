package placement

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/alicebob/miniredis/v2"

	"github.com/xbcio/xbc/extensions/coordination/lease"
	redisstore "github.com/xbcio/xbc/extensions/storage/redis"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// ── a controllable lease store ─────────────────────────────────────────────

// errStoreUnreachable stands in for whatever a backend reports when it cannot
// be reached. The tests assert on what placement does with it, not on how a
// particular client spells it.
var errStoreUnreachable = fmt.Errorf("dial tcp 127.0.0.1:6379: connect: connection refused")

// leaseEvent is one recorded call on the store, in the order it happened.
type leaseEvent struct {
	kind string
	key  string
}

// memoryLocker is a lease.Locker the tests can steer: they decide whether an
// acquisition succeeds, whether a renewal fails, and whether a renewal
// re-establishes a key that is already gone. Everything it does is recorded, so
// a test can assert on the order of store calls and not only on their effect.
type memoryLocker struct {
	mu     sync.Mutex
	held   map[string]string
	events []leaseEvent
	seq    int

	// deny refuses every acquisition, which is how "another process already
	// holds this" is spelled.
	deny bool
	// failAcquire makes TryAcquire report an unreachable store.
	failAcquire error
	// failOnAttempt, when positive, makes the Nth acquisition call fail and
	// every later one too, which is how a store that dies mid-decision is
	// spelled.
	failOnAttempt int
	// acquisitions counts TryAcquire calls, so failOnAttempt can be positioned.
	acquisitions int
	// claimants records what each acquisition published about the process
	// asking, so a test can pin the identity that reached the store and not only
	// the slot that was won. It records refused attempts too: what a process
	// calls itself does not depend on whether it wins.
	claimants []string
	// renewErr makes renewals fail.
	renewErr error
	// releaseErr makes releases fail, which is how a store that refuses the
	// handover is spelled.
	releaseErr error
	// renewReestablishes makes a renewal set its key again when it finds none,
	// which is the shape a renewal would have to have for a late renewal to
	// undo a release. No conforming backend does this; the test uses it to prove
	// the release guard covers the effect and not only the call.
	renewReestablishes bool
}

func newMemoryLocker() *memoryLocker {
	return &memoryLocker{held: make(map[string]string)}
}

func (l *memoryLocker) TryAcquire(_ context.Context, key, claimant string, _ time.Duration) (lease.Lease, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, leaseEvent{kind: "acquire", key: key})
	l.acquisitions++
	l.claimants = append(l.claimants, claimant)
	if l.failAcquire != nil {
		return nil, false, l.failAcquire
	}
	if l.failOnAttempt > 0 && l.acquisitions >= l.failOnAttempt {
		return nil, false, errStoreUnreachable
	}
	if l.deny {
		return nil, false, nil
	}
	if _, exists := l.held[key]; exists {
		return nil, false, nil
	}
	l.seq++
	// The token is composed the way the contract requires of a real backend: a
	// claimant prefix, when one was named, over a part no other acquisition
	// shares. Spelling it faithfully here is what lets the tests in this package
	// tell the published name apart from the value ownership is compared by,
	// without reaching for a real store to do it.
	owner := fmt.Sprintf("owner-%d", l.seq)
	if claimant != "" {
		owner = claimant + "/" + owner
	}
	l.held[key] = owner
	return &memoryLease{store: l, key: key, owner: owner}, true, nil
}

type memoryLease struct {
	store *memoryLocker
	key   string
	owner string
}

func (l *memoryLease) Key() string   { return l.key }
func (l *memoryLease) Owner() string { return l.owner }

func (l *memoryLease) Renew(_ context.Context, _ time.Duration) (bool, error) {
	l.store.mu.Lock()
	defer l.store.mu.Unlock()
	l.store.events = append(l.store.events, leaseEvent{kind: "renew", key: l.key})
	if l.store.renewErr != nil {
		return false, l.store.renewErr
	}
	if l.store.held[l.key] == l.owner {
		return true, nil
	}
	if l.store.renewReestablishes {
		l.store.held[l.key] = l.owner
		return true, nil
	}
	return false, nil
}

func (l *memoryLease) Release(_ context.Context) (bool, error) {
	l.store.mu.Lock()
	defer l.store.mu.Unlock()
	l.store.events = append(l.store.events, leaseEvent{kind: "release", key: l.key})
	if l.store.releaseErr != nil {
		return false, l.store.releaseErr
	}
	if l.store.held[l.key] != l.owner {
		return false, nil
	}
	delete(l.store.held, l.key)
	return true, nil
}

func (l *memoryLocker) holds(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, exists := l.held[key]
	return exists
}

// ownerOf is the token the store has in key, or "" when nothing holds it. It
// exists so a test can compare what a slot reports as its owner against what the
// store actually recorded, rather than against a value the test made up.
func (l *memoryLocker) ownerOf(key string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held[key]
}

// recordedClaimants copies the identity every acquisition published, in order.
func (l *memoryLocker) recordedClaimants() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.claimants...)
}

// heldKeys returns the keys this store currently has an owner for.
func (l *memoryLocker) heldKeys() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	keys := make([]string, 0, len(l.held))
	for key := range l.held {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (l *memoryLocker) attempts() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var keys []string
	for _, event := range l.events {
		if event.kind == "acquire" {
			keys = append(keys, event.key)
		}
	}
	return keys
}

// ── a recording wrapper around a real store ────────────────────────────────

// recordingLocker wraps a real lease.Locker and records every call that reaches
// the store, so a test can ask whether a renewal happened after a release
// rather than only whether the key survived.
type recordingLocker struct {
	inner lease.Locker

	mu     sync.Mutex
	events []leaseEvent
}

func newRecordingLocker(inner lease.Locker) *recordingLocker {
	return &recordingLocker{inner: inner}
}

func (l *recordingLocker) TryAcquire(ctx context.Context, key, claimant string, ttl time.Duration) (lease.Lease, bool, error) {
	won, acquired, err := l.inner.TryAcquire(ctx, key, claimant, ttl)
	l.record("acquire", key)
	if err != nil || !acquired {
		return nil, false, err
	}
	return &recordingLease{inner: won, recorder: l}, true, nil
}

func (l *recordingLocker) record(kind, key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, leaseEvent{kind: kind, key: key})
}

// renewCount counts the renewals this store recorded for one key.
func (l *memoryLocker) renewCount(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for _, event := range l.events {
		if event.kind == "renew" && event.key == key {
			count++
		}
	}
	return count
}

// renewAfterRelease reports whether a renewal for key was recorded after the
// last release of that key, and separately whether a release was recorded at
// all -- so a test can tell "the guard held" apart from "nothing was released".
func (l *recordingLocker) renewAfterRelease(key string) (released bool, renewed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	releasedAt := -1
	for index, event := range l.events {
		if event.kind == "release" && event.key == key {
			releasedAt = index
		}
	}
	if releasedAt < 0 {
		return false, false
	}
	for _, event := range l.events[releasedAt+1:] {
		if event.kind == "renew" && event.key == key {
			return true, true
		}
	}
	return true, false
}

func (l *recordingLocker) renewals(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for _, event := range l.events {
		if event.kind == "renew" && event.key == key {
			count++
		}
	}
	return count
}

type recordingLease struct {
	inner    lease.Lease
	recorder *recordingLocker
}

func (l *recordingLease) Key() string   { return l.inner.Key() }
func (l *recordingLease) Owner() string { return l.inner.Owner() }

func (l *recordingLease) Renew(ctx context.Context, ttl time.Duration) (bool, error) {
	owned, err := l.inner.Renew(ctx, ttl)
	l.recorder.record("renew", l.inner.Key())
	return owned, err
}

func (l *recordingLease) Release(ctx context.Context) (bool, error) {
	released, err := l.inner.Release(ctx)
	l.recorder.record("release", l.inner.Key())
	return released, err
}

// ── a Redis-backed store ───────────────────────────────────────────────────

func newMiniredis(t *testing.T) (*miniredis.Miniredis, *goredis.Client) {
	t.Helper()
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return server, client
}

func newRedisLocker(t *testing.T, client *goredis.Client) lease.Locker {
	t.Helper()
	locker, err := redisstore.NewLocker(client)
	if err != nil {
		t.Fatalf("redis.NewLocker: %v", err)
	}
	return locker
}

// ── a runtime host for the managed loops ───────────────────────────────────

// testHost is the smallest plugin.RuntimeHost that can run this plugin's
// lifecycle. It runs submitted tasks on real goroutines, exactly as the runtime
// does, so the tests exercise the same concurrency the product has; everything
// else about it is observable state a test can assert on.
type testHost struct {
	executionCtx context.Context
	cancel       context.CancelFunc
	logger       log.Logger
	gate         chan struct{}

	mu     sync.Mutex
	tasks  sync.WaitGroup
	stopCh chan string
}

var _ plugin.RuntimeHost = (*testHost)(nil)

func newTestHost() *testHost {
	executionCtx, cancel := context.WithCancel(context.Background())
	return &testHost{
		executionCtx: executionCtx,
		cancel:       cancel,
		logger:       log.Nop(),
		gate:         make(chan struct{}),
		stopCh:       make(chan string, 8),
	}
}

func (h *testHost) ExecutionContext() context.Context { return h.executionCtx }
func (h *testHost) Logger() log.Logger                { return h.logger }
func (h *testHost) TrafficGate() <-chan struct{}      { return h.gate }

func (h *testHost) SubmitTask(_ plugin.Identity, fn func(context.Context), _ bool) bool {
	h.mu.Lock()
	h.tasks.Add(1)
	h.mu.Unlock()
	go func() {
		defer h.tasks.Done()
		fn(h.executionCtx)
	}()
	return true
}

func (h *testHost) RequestShutdown(_ plugin.Identity, reason string) bool {
	select {
	case h.stopCh <- reason:
	default:
	}
	return true
}

// context builds the lifecycle Context one plugin instance receives.
func (h *testHost) context() *plugin.Context {
	return plugin.NewRuntimeContext(h, plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
}

func (h *testHost) openGate() { close(h.gate) }

// requestStop plays the part the runtime plays when the process is asked to
// stop: the execution context is cancelled first, and the lifecycle hooks run
// afterwards.
func (h *testHost) requestStop() { h.cancel() }

func (h *testHost) shutdownReason(t *testing.T) string {
	t.Helper()
	select {
	case reason := <-h.stopCh:
		return reason
	case <-time.After(5 * time.Second):
		t.Fatal("the plugin never asked for a restart")
		return ""
	}
}

func (h *testHost) awaitTasks(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		h.tasks.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("managed tasks did not return")
	}
}

// await polls condition until it holds. The managed loops are real goroutines,
// so a test that wants to observe what they did has to wait for them rather
// than assume a tick has landed.
func await(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

// ── shared requests ────────────────────────────────────────────────────────

// workloadRequest is a request from a caller with no process identity to offer,
// which is what a direct caller outside a run has. Tests about the claimant use
// instanceRequest instead, so the two cases stay visibly different.
func workloadRequest(workloads ...plugin.Workload) plugin.PlacementRequest {
	return plugin.PlacementRequest{Workloads: workloads}
}

// instanceRequest is a request as the runtime makes it: carrying the identity
// this process settled on while binding configuration.
func instanceRequest(instance string, workloads ...plugin.Workload) plugin.PlacementRequest {
	return plugin.PlacementRequest{Workloads: workloads, Instance: instance}
}

func ordinary(key plugin.WorkloadKey, replicas int) plugin.Workload {
	return plugin.Workload{Key: key, Replicas: replicas}
}

func exclusive(key plugin.WorkloadKey, replicas int) plugin.Workload {
	return plugin.Workload{Key: key, Replicas: replicas, Exclusive: true}
}

func mustNew(t *testing.T, locker lease.Locker, options ...Option) *Placement {
	t.Helper()
	value, err := New(locker, options...)
	if err != nil {
		t.Fatalf("placement.New: %v", err)
	}
	return value
}
