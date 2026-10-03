package async

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAntsExecutorPoolIsReleasedOnStop checks that Pool.stop releases the
// underlying ants pool (via antsExecutor.Release) rather than leaving it
// resident, and that drain -- unlike stop -- never does so: AGENTS.md
// requires every resource a dependent might still use to stay open through
// Drain and only wind down in Stop.
func TestAntsExecutorPoolIsReleasedOnStop(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Executor = ExecutorAnts
	cfg.MaxConcurrency = 4
	pool := newTestPool(t, cfg)

	exec, ok := pool.exec.(*antsExecutor)
	require.True(t, ok, "pool.exec must be an *antsExecutor when executor: ants is configured")
	assert.False(t, exec.pool.IsClosed(), "ants pool must start open")

	require.NoError(t, pool.drain(context.Background()))
	assert.False(t, exec.pool.IsClosed(), "Drain must not release the ants pool")

	require.NoError(t, pool.stop(context.Background()))
	assert.True(t, exec.pool.IsClosed(), "Stop must release the ants pool")
}

// TestAntsExecutorReleaseErrorIsReportedByStop confirms stop surfaces
// whatever ReleaseContext/ReleaseTimeout reports, by giving stop's ctx less
// time than a still-running task needs: once stop's own wait for inFlight is
// satisfied (because stop cancels the task's context, which this task reacts
// to immediately) Release itself still gets an already-near-expired ctx, so
// this instead directly exercises antsExecutor.Release with an
// already-expired context to confirm the error propagates up through Go's
// Release contract rather than being swallowed.
func TestAntsExecutorReleaseErrorIsReportedByStop(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Executor = ExecutorAnts
	cfg.MaxConcurrency = 2
	exec, err := newAntsExecutor(cfg.MaxConcurrency, cfg.Ants, nil)
	require.NoError(t, err)

	release := make(chan struct{})
	require.NoError(t, exec.Go(func() { <-release }))
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	time.Sleep(5 * time.Millisecond) // ensure ctx has actually expired

	err = exec.Release(ctx)
	assert.Error(t, err, "Release must report a context that expired before the running worker returned")
}

// TestAntsExecutorNoPprofLabelLeaksBetweenReusedWorkers proves that ants
// reusing a goroutine across tasks does not leak Pool.runTask's "async_task"
// pprof label from one task to the next.
//
// pprof labels live in two places: the context value pprof.Do's own f
// receives (always correct by construction -- WithLabels/Do build that ctx
// fresh every call, so reading a label back off it never actually tests
// Pool's own correctness) and the runtime's per-goroutine label set that
// SetGoroutineLabels mutates directly (runtime_setProfLabel), which is what
// a real profile samples and stays goroutine-local, carried across whatever
// that goroutine runs next until something overwrites or restores it. A
// leak -- Pool skipping pprof.Do per task, or ants somehow not restoring
// between tasks -- would only be visible there.
//
// The text goroutine dump (debug=1 or 2) does not carry labels at all (a
// known runtime/pprof limitation: see golang/go#63712 and #74964), so this
// reads the structured protobuf profile (debug=0) instead: labels there are
// recorded as literal strings in the profile's string table, which is
// enough to detect presence/absence without a protobuf decoder.
func TestAntsExecutorNoPprofLabelLeaksBetweenReusedWorkers(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Executor = ExecutorAnts
	cfg.MaxConcurrency = 1
	pool := newTestPool(t, cfg)
	t.Cleanup(func() { _ = pool.stop(context.Background()) })

	// Use names unlikely to appear anywhere else in a goroutine profile
	// (symbol names, paths, etc.) so a substring match is unambiguous.
	const firstLabel = "async-test-label-first-task-f7c3"
	const secondLabel = "async-test-label-second-task-9b21"

	firstStarted := make(chan struct{})
	firstRelease := make(chan struct{})
	require.NoError(t, pool.Spawn(context.Background(), firstLabel, func(context.Context) {
		close(firstStarted)
		<-firstRelease
	}))
	<-firstStarted
	waitForCondition(t, func() bool {
		return goroutineProfileContains(t, firstLabel)
	}, "goroutine profile to show the running first task's label")
	close(firstRelease)
	waitForCondition(t, func() bool { return pool.Stats().Running == 0 }, "first task to finish")

	// The worker goroutine is now idle, parked inside ants waiting for its
	// next task (see worker.go's `for fn := range w.task`), with
	// Pool.runTask's deferred SetGoroutineLabels(ctx) already having
	// restored it to whatever it carried before the first task ran (nil,
	// for a freshly spawned worker): its label must not still read the
	// first task's name while idle.
	waitForCondition(t, func() bool {
		return !goroutineProfileContains(t, firstLabel)
	}, "the idle worker goroutine to no longer carry the finished task's label")

	// Run the second task on the same (size-1 pool, necessarily reused)
	// worker and confirm the profile shows only its own label, never the
	// first's.
	secondStarted := make(chan struct{})
	secondRelease := make(chan struct{})
	require.NoError(t, pool.Spawn(context.Background(), secondLabel, func(context.Context) {
		close(secondStarted)
		<-secondRelease
	}))
	<-secondStarted
	waitForCondition(t, func() bool {
		return goroutineProfileContains(t, secondLabel)
	}, "goroutine profile to show the running second task's label")
	assert.False(t, goroutineProfileContains(t, firstLabel),
		"the reused worker goroutine must not still carry the first task's label while running the second")
	close(secondRelease)
}

// goroutineProfileContains reports whether the live, structured (debug=0)
// goroutine profile -- gzip-compressed protobuf, whose string table holds
// every pprof label value as a literal string -- contains needle.
func goroutineProfileContains(t *testing.T, needle string) bool {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, pprof.Lookup("goroutine").WriteTo(&buf, 0))
	reader, err := gzip.NewReader(&buf)
	require.NoError(t, err)
	raw, err := io.ReadAll(reader)
	require.NoError(t, err)
	return bytes.Contains(raw, []byte(needle))
}

// TestAntsExecutorErrPoolOverloadNeverObservedUnderSaturationStress stresses
// an ants-backed Pool far past its MaxConcurrency with concurrent Spawn
// callers under -race, asserting Go never surfaces the "ants pool rejected a
// task its own size should have admitted" error: Pool's own semaphore must
// always have reserved a slot before any task reaches antsExecutor.Go, so
// ants.ErrPoolOverload is never actually observed in practice.
func TestAntsExecutorErrPoolOverloadNeverObservedUnderSaturationStress(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Executor = ExecutorAnts
	cfg.MaxConcurrency = 8
	cfg.QueueCapacity = 32
	cfg.SubmitTimeout = 500 * time.Millisecond
	pool := newTestPool(t, cfg)
	t.Cleanup(func() { _ = pool.stop(context.Background()) })

	const callers = 64
	const spawnsPerCaller = 50
	var wg sync.WaitGroup
	var overloadObserved atomic.Bool
	var completed atomic.Int64

	for c := 0; c < callers; c++ {
		wg.Add(1)
		go func(caller int) {
			defer wg.Done()
			for i := 0; i < spawnsPerCaller; i++ {
				name := fmt.Sprintf("stress-%d-%d", caller, i)
				done := make(chan struct{})
				err := pool.Spawn(context.Background(), name, func(context.Context) {
					completed.Add(1)
					close(done)
				})
				if err == nil {
					<-done
					continue
				}
				if err == ErrSaturated { //nolint:errorlint // identity check against the sentinel is intentional here
					continue // expected under enough pressure; not what this test is checking for
				}
				// Any other error -- in particular Go's wrapped
				// ants.ErrPoolOverload -- is the invariant violation this
				// test exists to catch.
				overloadObserved.Store(true)
				t.Errorf("unexpected Spawn error under saturation stress: %v", err)
			}
		}(c)
	}
	wg.Wait()

	assert.False(t, overloadObserved.Load(), "ErrPoolOverload (or any executor-rejection error) must never be observed: Pool's semaphore already reserves every slot before Go is called")
	assert.Greater(t, completed.Load(), int64(0), "at least some stress tasks should have completed")
}
