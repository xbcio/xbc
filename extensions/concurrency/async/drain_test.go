package async

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDrainWaitsForRunningAndQueuedTasksUnderLiveContext(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrency = 1
	cfg.QueueCapacity = 5
	pool := newTestPool(t, cfg)

	running := newBlockingTask()
	queued := newBlockingTask()
	require.NoError(t, pool.Spawn(context.Background(), "running", running.run))
	waitForCondition(t, func() bool { return pool.Stats().Running == 1 }, "running task to start")
	require.NoError(t, pool.Spawn(context.Background(), "queued", queued.run))
	waitForCondition(t, func() bool { return pool.Stats().Queued == 1 }, "second task to queue")

	drainDone := make(chan error, 1)
	go func() { drainDone <- pool.drain(context.Background()) }()

	// New Spawn calls are refused while draining.
	waitForCondition(t, func() bool {
		return pool.Spawn(context.Background(), "late", func(context.Context) {}) != nil
	}, "spawn to be refused during drain")
	assert.ErrorIs(t, pool.Spawn(context.Background(), "late", func(context.Context) {}), ErrShuttingDown)

	close(running.release)
	// queued is now dispatched into the freed slot and blocks on its own
	// release; waitStarted confirms the dispatch rather than waiting for
	// Queued==0, which would deadlock here since queued itself is still
	// running.
	select {
	case <-queued.started:
	case <-time.After(2 * time.Second):
		t.Fatal("queued task did not start once the running slot freed")
	}
	close(queued.release)

	select {
	case err := <-drainDone:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Drain did not return after accepted work finished")
	}

	assert.ErrorIs(t, pool.Spawn(context.Background(), "after-drain", func(context.Context) {}), ErrShuttingDown)
	require.NoError(t, pool.stop(context.Background()))
}

func TestExpiredDrainLeavesTasksRunningAndStopThenCancelsThemAndDiscardsQueue(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrency = 1
	cfg.QueueCapacity = 5
	pool := newTestPool(t, cfg)

	running := newBlockingTask()
	require.NoError(t, pool.Spawn(context.Background(), "running", running.run))
	waitForCondition(t, func() bool { return pool.Stats().Running == 1 }, "running task to start")

	queuedStarted := make(chan struct{}, 1)
	require.NoError(t, pool.Spawn(context.Background(), "queued", func(context.Context) {
		select {
		case queuedStarted <- struct{}{}:
		default:
		}
	}))
	waitForCondition(t, func() bool { return pool.Stats().Queued == 1 }, "queued task to be queued")

	drainCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := pool.drain(drainCtx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	// The running task is left running, not cancelled, by the expired drain.
	select {
	case <-running.finished:
		t.Fatal("drain must not cancel work that is still running")
	case <-time.After(50 * time.Millisecond):
	}

	// Stop now cancels the running task and discards the queue.
	stopErr := pool.stop(context.Background())
	assert.NoError(t, stopErr)

	select {
	case taskErr := <-running.finished:
		assert.ErrorIs(t, taskErr, context.Canceled, "Stop must cancel the task the drain left running")
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not cancel the running task")
	}

	select {
	case <-queuedStarted:
		t.Fatal("Stop must discard the queued task rather than start it")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestAwaitTerminationFalseDrainDoesNotWaitAndStopCancelsRunning(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrency = 1
	cfg.Shutdown.AwaitTermination = false
	pool := newTestPool(t, cfg)

	running := newBlockingTask()
	require.NoError(t, pool.Spawn(context.Background(), "running", running.run))
	waitForCondition(t, func() bool { return pool.Stats().Running == 1 }, "running task to start")

	start := time.Now()
	err := pool.drain(context.Background())
	elapsed := time.Since(start)
	assert.NoError(t, err)
	assert.Less(t, elapsed, 500*time.Millisecond, "Drain with await_termination=false must return immediately")

	select {
	case <-running.finished:
		t.Fatal("Drain with await_termination=false must not itself cancel running work")
	case <-time.After(50 * time.Millisecond):
	}

	require.NoError(t, pool.stop(context.Background()))
	select {
	case taskErr := <-running.finished:
		assert.ErrorIs(t, taskErr, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not cancel the running task")
	}
}

func TestAwaitTerminationPeriodCapsTheWait(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrency = 1
	cfg.Shutdown.AwaitTerminationPeriod = 50 * time.Millisecond
	pool := newTestPool(t, cfg)
	t.Cleanup(func() { _ = pool.stop(context.Background()) })

	running := newBlockingTask()
	defer close(running.release)
	require.NoError(t, pool.Spawn(context.Background(), "running", running.run))
	waitForCondition(t, func() bool { return pool.Stats().Running == 1 }, "running task to start")

	start := time.Now()
	// A long-lived ctx; the configured period must still cap the wait.
	err := pool.drain(context.Background())
	elapsed := time.Since(start)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, elapsed, time.Second)
	assert.GreaterOrEqual(t, elapsed, 40*time.Millisecond)
}

func TestDrainIsIdempotentAndReplaysTheSameResult(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrency = 1
	cfg.Shutdown.AwaitTerminationPeriod = 30 * time.Millisecond
	pool := newTestPool(t, cfg)
	t.Cleanup(func() { _ = pool.stop(context.Background()) })

	running := newBlockingTask()
	defer close(running.release)
	require.NoError(t, pool.Spawn(context.Background(), "running", running.run))
	waitForCondition(t, func() bool { return pool.Stats().Running == 1 }, "running task to start")

	first := pool.drain(context.Background())
	second := pool.drain(context.Background())
	assert.ErrorIs(t, first, context.DeadlineExceeded)
	assert.Equal(t, first, second)
}

func TestDrainBeforeInitOrStartIsSafeAndRefusesLaterOpen(t *testing.T) {
	pool := mustNewPool(t, DefaultConfig(), nil) // not yet "opened"/started

	err := pool.drain(context.Background())
	assert.NoError(t, err)

	// A pool drained before Start refuses to (re)open admission.
	pool.open()
	err = pool.Spawn(context.Background(), "x", func(context.Context) {})
	assert.ErrorIs(t, err, ErrShuttingDown)

	require.NoError(t, pool.stop(context.Background()))
}

func TestConcurrentDrainAndStop(t *testing.T) {
	pool := newTestPool(t, DefaultConfig())

	running := newBlockingTask()
	require.NoError(t, pool.Spawn(context.Background(), "running", running.run))
	waitForCondition(t, func() bool { return pool.Stats().Running == 1 }, "running task to start")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = pool.drain(context.Background())
	}()
	go func() {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
		_ = pool.stop(context.Background())
	}()
	close(running.release)
	wg.Wait()
}

func TestStopIsIdempotent(t *testing.T) {
	pool := newTestPool(t, DefaultConfig())
	require.NoError(t, pool.stop(context.Background()))
	require.NoError(t, pool.stop(context.Background()))
}

func TestStopDiscardsQueuedTasksAndLogsThem(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrency = 1
	cfg.QueueCapacity = 5
	pool := newTestPool(t, cfg)

	running := newBlockingTask()
	require.NoError(t, pool.Spawn(context.Background(), "running", running.run))
	waitForCondition(t, func() bool { return pool.Stats().Running == 1 }, "running task to start")

	started := make(chan struct{}, 1)
	require.NoError(t, pool.Spawn(context.Background(), "never-runs", func(context.Context) {
		select {
		case started <- struct{}{}:
		default:
		}
	}))
	waitForCondition(t, func() bool { return pool.Stats().Queued == 1 }, "task to queue")

	require.NoError(t, pool.stop(context.Background()))
	select {
	case <-started:
		t.Fatal("Stop must discard a queued task rather than run it")
	case <-time.After(100 * time.Millisecond):
	}
	close(running.release)
}
