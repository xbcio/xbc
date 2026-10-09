package async

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
)

// TestPoolChargesItsWorkloadsQuotaForEveryExecutedTask pins the accounting
// down: a task the pool actually runs takes a unit before its body and gives it
// back after, so max_goroutines bounds the pool's concurrent work rather than
// the goroutines the framework started for it.
func TestPoolChargesItsWorkloadsQuotaForEveryExecutedTask(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrency = 2
	quota := &recordingAdmission{}
	host := newFakeHost()
	host.admission = quota
	pool := mustNewPool(t, cfg, log.Nop())
	require.NoError(t, initPool(pool, runtimeContext(host, "")))
	t.Cleanup(func() { require.NoError(t, pool.stop(context.Background())) })

	heldWhileRunning := make(chan int, 1)
	require.NoError(t, pool.Spawn(context.Background(), "first", func(context.Context) {
		heldWhileRunning <- quota.balance()
	}))
	select {
	case held := <-heldWhileRunning:
		require.Equal(t, 1, held, "the task body must run with its unit taken")
	case <-time.After(2 * time.Second):
		t.Fatal("the task never ran")
	}
	waitForCondition(t, func() bool { return quota.balance() == 0 }, "the unit to be given back")
	require.Equal(t, []string{"acquire", "release"}, quota.events())
}

// TestPoolTaskWaitsForTheQuotaInsteadOfBeingRefused checks the backpressure the
// quota is for: the executor's slot is held while the task waits for a unit,
// and no task body starts while the workload is at its limit.
func TestPoolTaskWaitsForTheQuotaInsteadOfBeingRefused(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrency = 4
	quota := newGatedAdmission(1)
	host := newFakeHost()
	host.admission = quota
	pool := mustNewPool(t, cfg, log.Nop())
	require.NoError(t, initPool(pool, runtimeContext(host, "")))
	t.Cleanup(func() { require.NoError(t, pool.stop(context.Background())) })

	running := make(chan struct{}, 4)
	release := make(chan struct{})
	require.NoError(t, pool.Spawn(context.Background(), "holder", func(context.Context) {
		running <- struct{}{}
		<-release
	}))
	<-running
	waitForCondition(t, func() bool { return quota.taken() == 1 }, "the first unit to be taken")

	require.NoError(t, pool.Spawn(context.Background(), "waiting", func(context.Context) { running <- struct{}{} }))
	select {
	case <-running:
		t.Fatal("the second task ran while its workload's quota was fully taken")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-running:
	case <-time.After(2 * time.Second):
		t.Fatal("the waiting task did not run once a unit came free")
	}
}

// TestPoolTaskParkedOnItsQuotaIsAbandonedAtStop covers the wait that ends with
// the pool: the task never runs, stop still returns, and the pool reports what
// it left rather than hanging on a quota that will never come free.
func TestPoolTaskParkedOnItsQuotaIsAbandonedAtStop(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrency = 2
	quota := newGatedAdmission(0)
	host := newFakeHost()
	host.admission = quota
	pool := mustNewPool(t, cfg, log.Nop())
	require.NoError(t, initPool(pool, runtimeContext(host, "")))

	ran := make(chan struct{}, 1)
	require.NoError(t, pool.Spawn(context.Background(), "parked", func(context.Context) { ran <- struct{}{} }))
	waitForCondition(t, func() bool { return quota.waiting() == 1 }, "the task to park on the quota")

	stopped := make(chan error, 1)
	go func() { stopped <- pool.stop(context.Background()) }()
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not return while a task was parked on its workload's quota")
	}
	select {
	case <-ran:
		t.Fatal("a task ran after the pool stopped")
	default:
	}
}

// TestPoolWithoutAWorkloadQuotaRunsEveryTask keeps the unbounded case honest: a
// pool whose plugin belongs to no workload is bounded by its own concurrency and
// queue capacity alone, and every task still runs.
func TestPoolWithoutAWorkloadQuotaRunsEveryTask(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrency = 2
	pool := newTestPool(t, cfg)
	t.Cleanup(func() { require.NoError(t, pool.stop(context.Background())) })

	done := make(chan struct{}, 1)
	require.NoError(t, pool.Spawn(context.Background(), "unbounded", func(context.Context) { done <- struct{}{} }))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a task did not run in a pool with no workload quota")
	}
}

// recordingAdmission counts what a task charged, in order, so a test can tell
// the unit was held for the whole task rather than taken and returned early.
type recordingAdmission struct {
	mu    sync.Mutex
	order []string
	held  int
}

func (a *recordingAdmission) Acquire(context.Context) (func(), error) {
	a.mu.Lock()
	a.order = append(a.order, "acquire")
	a.held++
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		a.held--
		a.order = append(a.order, "release")
		a.mu.Unlock()
	}, nil
}

func (a *recordingAdmission) events() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.order...)
}

func (a *recordingAdmission) balance() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.held
}

// gatedAdmission is a quota of a fixed size whose units are given back only by
// the task that took them. size 0 is a quota that never grants anything.
type gatedAdmission struct {
	mu      sync.Mutex
	size    int
	held    int
	parked  int
	changed chan struct{}
}

func newGatedAdmission(size int) *gatedAdmission {
	return &gatedAdmission{size: size, changed: make(chan struct{})}
}

func (a *gatedAdmission) Acquire(ctx context.Context) (func(), error) {
	for {
		a.mu.Lock()
		if a.held < a.size {
			a.held++
			a.mu.Unlock()
			return func() {
				a.mu.Lock()
				a.held--
				close(a.changed)
				a.changed = make(chan struct{})
				a.mu.Unlock()
			}, nil
		}
		a.parked++
		changed := a.changed
		a.mu.Unlock()
		select {
		case <-changed:
			a.mu.Lock()
			a.parked--
			a.mu.Unlock()
		case <-ctx.Done():
			a.mu.Lock()
			a.parked--
			a.mu.Unlock()
			return nil, ctx.Err()
		}
	}
}

func (a *gatedAdmission) taken() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.held
}

func (a *gatedAdmission) waiting() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.parked
}
