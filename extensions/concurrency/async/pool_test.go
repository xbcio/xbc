package async

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpawnRunsTask(t *testing.T) {
	pool := newTestPool(t, DefaultConfig())
	t.Cleanup(func() { _ = pool.Stop(context.Background()) })

	done := make(chan struct{})
	err := pool.Spawn(context.Background(), "greet", func(context.Context) { close(done) })
	require.NoError(t, err)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("task did not run")
	}
}

type ctxKey string

func TestTaskContextSurvivesSpawnCancellationButCarriesValues(t *testing.T) {
	pool := newTestPool(t, DefaultConfig())
	t.Cleanup(func() { _ = pool.Stop(context.Background()) })

	spawnCtx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey("trace"), "abc123"))

	started := make(chan struct{})
	result := make(chan struct {
		value    any
		canceled bool
	}, 1)
	err := pool.Spawn(spawnCtx, "survive-cancel", func(taskCtx context.Context) {
		close(started)
		<-time.After(50 * time.Millisecond) // give the test time to cancel spawnCtx
		result <- struct {
			value    any
			canceled bool
		}{value: taskCtx.Value(ctxKey("trace")), canceled: taskCtx.Err() != nil}
	})
	require.NoError(t, err)

	<-started
	cancel() // cancel the Spawn ctx; the task's own ctx must not observe it

	select {
	case r := <-result:
		assert.Equal(t, "abc123", r.value, "task context must carry the spawn context's values")
		assert.False(t, r.canceled, "task context must not be cancelled when the spawn ctx is cancelled")
	case <-time.After(2 * time.Second):
		t.Fatal("task did not complete")
	}
}

func TestSpawnRejectsInvalidArguments(t *testing.T) {
	pool := newTestPool(t, DefaultConfig())
	t.Cleanup(func() { _ = pool.Stop(context.Background()) })

	assert.ErrorIs(t, pool.Spawn(context.Background(), "", func(context.Context) {}), ErrInvalidTask)
	assert.ErrorIs(t, pool.Spawn(context.Background(), "name", nil), ErrInvalidTask)
}

func TestMaxConcurrencyIsNeverExceeded(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrency = 3
	cfg.QueueCapacity = 100
	pool := newTestPool(t, cfg)
	t.Cleanup(func() { _ = pool.Stop(context.Background()) })

	const tasks = 20
	var running atomic.Int64
	var maxObserved atomic.Int64
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(tasks)

	for i := 0; i < tasks; i++ {
		err := pool.Spawn(context.Background(), "concurrent", func(context.Context) {
			defer wg.Done()
			n := running.Add(1)
			for {
				max := maxObserved.Load()
				if n <= max || maxObserved.CompareAndSwap(max, n) {
					break
				}
			}
			<-release
			running.Add(-1)
		})
		require.NoError(t, err)
	}

	time.Sleep(100 * time.Millisecond)
	assert.LessOrEqual(t, maxObserved.Load(), int64(3))
	close(release)
	wg.Wait()
}

func TestQueueUsedThenErrSaturated(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrency = 1
	cfg.QueueCapacity = 1
	pool := newTestPool(t, cfg)
	t.Cleanup(func() { _ = pool.Stop(context.Background()) })

	release := make(chan struct{})
	require.NoError(t, pool.Spawn(context.Background(), "running", func(context.Context) { <-release }))
	waitForCondition(t, func() bool { return pool.Stats().Running == 1 }, "first task to start running")

	// Second task occupies the one queue slot.
	require.NoError(t, pool.Spawn(context.Background(), "queued", func(context.Context) {}))
	waitForCondition(t, func() bool { return pool.Stats().Queued == 1 }, "second task to be queued")

	// Third task finds no running slot and no queue space, and no wait is
	// configured, so it is rejected immediately.
	err := pool.Spawn(context.Background(), "overflow", func(context.Context) {})
	assert.ErrorIs(t, err, ErrSaturated)

	close(release)
}

func TestSubmitTimeoutWaitsThenSucceedsWhenSlotFrees(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrency = 1
	cfg.QueueCapacity = 0
	cfg.SubmitTimeout = 2 * time.Second
	pool := newTestPool(t, cfg)
	t.Cleanup(func() { _ = pool.Stop(context.Background()) })

	release := make(chan struct{})
	require.NoError(t, pool.Spawn(context.Background(), "occupying", func(context.Context) { <-release }))
	waitForCondition(t, func() bool { return pool.Stats().Running == 1 }, "occupying task to start")

	done := make(chan error, 1)
	go func() {
		done <- pool.Spawn(context.Background(), "waits-for-slot", func(context.Context) {})
	}()

	time.Sleep(100 * time.Millisecond) // let the waiting Spawn actually park
	close(release)

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Spawn did not succeed once a slot freed")
	}
}

func TestSubmitTimeoutFailsAfterTimeout(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrency = 1
	cfg.QueueCapacity = 0
	cfg.SubmitTimeout = 100 * time.Millisecond
	pool := newTestPool(t, cfg)
	t.Cleanup(func() { _ = pool.Stop(context.Background()) })

	release := make(chan struct{})
	defer close(release)
	require.NoError(t, pool.Spawn(context.Background(), "occupying", func(context.Context) { <-release }))
	waitForCondition(t, func() bool { return pool.Stats().Running == 1 }, "occupying task to start")

	start := time.Now()
	err := pool.Spawn(context.Background(), "times-out", func(context.Context) {})
	elapsed := time.Since(start)
	assert.ErrorIs(t, err, ErrSaturated)
	assert.GreaterOrEqual(t, elapsed, 90*time.Millisecond)
}

func TestSpawnCtxDeadlineShorterThanSubmitTimeoutWins(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrency = 1
	cfg.QueueCapacity = 0
	cfg.SubmitTimeout = 10 * time.Second
	pool := newTestPool(t, cfg)
	t.Cleanup(func() { _ = pool.Stop(context.Background()) })

	release := make(chan struct{})
	defer close(release)
	require.NoError(t, pool.Spawn(context.Background(), "occupying", func(context.Context) { <-release }))
	waitForCondition(t, func() bool { return pool.Stats().Running == 1 }, "occupying task to start")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := pool.Spawn(ctx, "short-ctx", func(context.Context) {})
	elapsed := time.Since(start)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, elapsed, 2*time.Second)
}

func TestPanicInTaskIsRecoveredAndPoolKeepsWorking(t *testing.T) {
	pool := newTestPool(t, DefaultConfig())
	t.Cleanup(func() { _ = pool.Stop(context.Background()) })

	panicked := make(chan struct{})
	require.NoError(t, pool.Spawn(context.Background(), "panics", func(context.Context) {
		close(panicked)
		panic("boom")
	}))
	select {
	case <-panicked:
	case <-time.After(2 * time.Second):
		t.Fatal("panicking task did not run")
	}

	waitForCondition(t, func() bool { return pool.Stats().Running == 0 }, "panicking task to be accounted as finished")

	done := make(chan struct{})
	require.NoError(t, pool.Spawn(context.Background(), "after-panic", func(context.Context) { close(done) }))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pool stopped working after a task panicked")
	}
}

func TestSpawnOnNilPoolReturnsErrNotInstalled(t *testing.T) {
	var pool *Pool
	err := pool.Spawn(context.Background(), "x", func(context.Context) {})
	assert.ErrorIs(t, err, ErrNotInstalled)
}

func TestSpawnRequiresNonNilContext(t *testing.T) {
	pool := newTestPool(t, DefaultConfig())
	t.Cleanup(func() { _ = pool.Stop(context.Background()) })
	//nolint:staticcheck // intentionally passing nil to exercise the guard
	err := pool.Spawn(nil, "x", func(context.Context) {})
	require.Error(t, err)
}

func TestSpawnRejectsAlreadyExpiredContext(t *testing.T) {
	pool := newTestPool(t, DefaultConfig())
	t.Cleanup(func() { _ = pool.Stop(context.Background()) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := pool.Spawn(ctx, "x", func(context.Context) {})
	assert.ErrorIs(t, err, context.Canceled)
}

func TestQueuedTasksRunAsSlotsFree(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrency = 1
	cfg.QueueCapacity = 5
	pool := newTestPool(t, cfg)
	t.Cleanup(func() { _ = pool.Stop(context.Background()) })

	var order []int
	var mu sync.Mutex
	releases := make([]chan struct{}, 3)
	for i := range releases {
		releases[i] = make(chan struct{})
	}

	for i := 0; i < 3; i++ {
		i := i
		require.NoError(t, pool.Spawn(context.Background(), "ordered", func(context.Context) {
			<-releases[i]
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
		}))
	}
	waitForCondition(t, func() bool { return pool.Stats().Running == 1 && pool.Stats().Queued == 2 }, "two tasks queued behind the first")

	close(releases[0])
	waitForCondition(t, func() bool { return pool.Stats().Queued == 1 }, "second task dequeued")
	close(releases[1])
	waitForCondition(t, func() bool { return pool.Stats().Queued == 0 }, "third task dequeued")
	close(releases[2])

	waitForCondition(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 3
	}, "all queued tasks to finish")
}

func TestErrorsAreDistinct(t *testing.T) {
	assert.False(t, errors.Is(ErrNotInstalled, ErrShuttingDown))
	assert.False(t, errors.Is(ErrShuttingDown, ErrSaturated))
	assert.False(t, errors.Is(ErrSaturated, ErrInvalidTask))
}
