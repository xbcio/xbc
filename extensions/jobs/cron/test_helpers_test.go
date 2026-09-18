package cron

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xbcio/xbc/extensions/coordination/lease"
	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

type testHost struct {
	mu sync.Mutex

	ctx            context.Context
	cancel         context.CancelFunc
	gate           chan struct{}
	gateOnce       sync.Once
	accepting      bool
	admissionLimit int
	submitted      int
	critical       int
	tasks          sync.WaitGroup
}

var _ plugin.RuntimeHost = (*testHost)(nil)

func newTestHost() *testHost {
	ctx, cancel := context.WithCancel(context.Background())
	return &testHost{
		ctx:            ctx,
		cancel:         cancel,
		gate:           make(chan struct{}),
		accepting:      true,
		admissionLimit: -1,
	}
}

func (h *testHost) ExecutionContext() context.Context { return h.ctx }
func (*testHost) Logger() corelog.Logger              { return corelog.Nop() }
func (h *testHost) TrafficGate() <-chan struct{}      { return h.gate }
func (*testHost) RequestShutdown(plugin.Identity, string) bool {
	return false
}

func (h *testHost) SubmitTask(_ plugin.Identity, fn func(context.Context), critical bool) bool {
	h.mu.Lock()
	if !h.accepting || (h.admissionLimit >= 0 && h.submitted >= h.admissionLimit) {
		h.mu.Unlock()
		return false
	}
	h.submitted++
	if critical {
		h.critical++
	}
	h.tasks.Add(1)
	ctx := h.ctx
	h.mu.Unlock()

	go func() {
		defer h.tasks.Done()
		fn(ctx)
	}()
	return true
}

func (h *testHost) setAdmissionLimit(limit int) {
	h.mu.Lock()
	h.admissionLimit = limit
	h.mu.Unlock()
}

func (h *testHost) counts() (submitted, critical int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.submitted, h.critical
}

func (h *testHost) openTraffic() { h.gateOnce.Do(func() { close(h.gate) }) }

func (h *testHost) close() {
	h.mu.Lock()
	h.accepting = false
	h.mu.Unlock()
	h.cancel()
	h.tasks.Wait()
}

func testContext(host plugin.RuntimeHost) *plugin.Context {
	return plugin.NewRuntimeContext(host, plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
}

type testContributor struct{ jobs []Job }

func (p testContributor) Jobs() []Job { return append([]Job(nil), p.jobs...) }

type funcJob struct {
	name string
	spec string
	run  func(context.Context) error
}

func (j *funcJob) Name() string                  { return j.name }
func (j *funcJob) Spec() string                  { return j.spec }
func (j *funcJob) Run(ctx context.Context) error { return j.run(ctx) }

func contributorEntry(instance string, jobs ...Job) plugin.Entry[JobContributor] {
	return plugin.Entry[JobContributor]{
		Identity: plugin.Identity{Plugin: "worker", Instance: instance},
		Value:    testContributor{jobs: jobs},
	}
}

func newTestPlugin(
	t *testing.T,
	configure func(*Config),
	locker lease.Locker,
	jobs ...Job,
) *Plugin {
	t.Helper()
	config := defaultConfig()
	if configure != nil {
		configure(&config)
	}
	prepared, err := prepareConfig(config)
	if err != nil {
		t.Fatalf("prepareConfig() error = %v", err)
	}
	p, err := newConfiguredPlugin(prepared, []plugin.Entry[JobContributor]{contributorEntry("jobs", jobs...)}, locker, nil)
	if err != nil {
		t.Fatalf("newConfiguredPlugin() error = %v", err)
	}
	return p
}

func initTestPlugin(t *testing.T, host *testHost, configure func(*Config), locker lease.Locker, jobs ...Job) (*Plugin, *plugin.Context) {
	t.Helper()
	p := newTestPlugin(t, configure, locker, jobs...)
	ctx := testContext(host)
	if err := p.init(ctx); err != nil {
		t.Fatalf("init() error = %v", err)
	}
	return p, ctx
}

func stopTestPlugin(t *testing.T, p *Plugin, host *testHost) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.stop(ctx); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
	host.close()
}

func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func awaitCondition(t *testing.T, timeout time.Duration, condition func() bool, what string) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-poll.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func assertNoSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("unexpected signal: %s", what)
	default:
	}
}

type fakeLocker struct {
	mu       sync.Mutex
	next     uint64
	held     map[string]*fakeLease
	attempts chan struct{}
	renewed  chan struct{}
	released chan struct{}
	// claimants records the claimant of every acquisition, so a test can pin
	// what the scheduler publishes about itself rather than only that it locked.
	claimants []string
}

// recordedClaimants copies what every acquisition published about the process
// asking, under the lock the scheduler's own goroutines acquire through.
func (l *fakeLocker) recordedClaimants() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.claimants...)
}

func newFakeLocker() *fakeLocker {
	return &fakeLocker{
		held:     make(map[string]*fakeLease),
		attempts: make(chan struct{}, 128),
		renewed:  make(chan struct{}, 128),
		released: make(chan struct{}, 128),
	}
}

func (l *fakeLocker) TryAcquire(ctx context.Context, key, claimant string, ttl time.Duration) (lease.Lease, bool, error) {
	select {
	case <-ctx.Done():
		return nil, false, ctx.Err()
	default:
	}
	if ttl <= 0 {
		return nil, false, fmt.Errorf("invalid ttl %s", ttl)
	}
	l.attempts <- struct{}{}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.claimants = append(l.claimants, claimant)
	if _, occupied := l.held[key]; occupied {
		return nil, false, nil
	}
	l.next++
	lease := &fakeLease{locker: l, key: key, owner: fmt.Sprintf("owner-%d", l.next)}
	l.held[key] = lease
	return lease, true, nil
}

type fakeLease struct {
	locker *fakeLocker
	key    string
	owner  string
}

func (l *fakeLease) Key() string   { return l.key }
func (l *fakeLease) Owner() string { return l.owner }

func (l *fakeLease) Renew(ctx context.Context, _ time.Duration) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	default:
	}
	l.locker.mu.Lock()
	owned := l.locker.held[l.key] == l
	l.locker.mu.Unlock()
	if owned {
		l.locker.renewed <- struct{}{}
	}
	return owned, nil
}

func (l *fakeLease) Release(ctx context.Context) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	default:
	}
	l.locker.mu.Lock()
	owned := l.locker.held[l.key] == l
	if owned {
		delete(l.locker.held, l.key)
	}
	l.locker.mu.Unlock()
	if owned {
		l.locker.released <- struct{}{}
	}
	return owned, nil
}

func updateMax(maximum *atomic.Int32, candidate int32) {
	for {
		current := maximum.Load()
		if candidate <= current || maximum.CompareAndSwap(current, candidate) {
			return
		}
	}
}
