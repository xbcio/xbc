package async

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExecutorSharedBehavior runs the semantics every executor must honor
// identically against each entry in executorConfigs(): the concurrency cap,
// queueing, saturation, submit_timeout, panic recovery, drain's wait, an
// expired drain leaving work running, stop cancelling and discarding, and the
// process-global binding. Per-executor peculiarities (ants pool release on
// Stop, no pprof label leak across reused workers, ErrPoolOverload never
// observed) live in executor_ants_test.go instead: this file is only the
// behavior Pool promises regardless of which executor is installed.
func TestExecutorSharedBehavior(t *testing.T) {
	for name, baseCfg := range executorConfigs() {
		baseCfg := baseCfg
		t.Run(name, func(t *testing.T) {
			t.Run("MaxConcurrencyIsNeverExceeded", func(t *testing.T) {
				cfg := baseCfg
				cfg.MaxConcurrency = 3
				cfg.QueueCapacity = 100
				pool := newTestPool(t, cfg)
				t.Cleanup(func() { _ = pool.stop(context.Background()) })

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
			})

			t.Run("QueueUsedThenErrSaturated", func(t *testing.T) {
				cfg := baseCfg
				cfg.MaxConcurrency = 1
				cfg.QueueCapacity = 1
				pool := newTestPool(t, cfg)
				t.Cleanup(func() { _ = pool.stop(context.Background()) })

				release := make(chan struct{})
				require.NoError(t, pool.Spawn(context.Background(), "running", func(context.Context) { <-release }))
				waitForCondition(t, func() bool { return pool.Stats().Running == 1 }, "first task to start running")

				require.NoError(t, pool.Spawn(context.Background(), "queued", func(context.Context) {}))
				waitForCondition(t, func() bool { return pool.Stats().Queued == 1 }, "second task to be queued")

				err := pool.Spawn(context.Background(), "overflow", func(context.Context) {})
				assert.ErrorIs(t, err, ErrSaturated)

				close(release)
			})

			t.Run("SubmitTimeoutWaitsThenSucceedsWhenSlotFrees", func(t *testing.T) {
				cfg := baseCfg
				cfg.MaxConcurrency = 1
				cfg.QueueCapacity = 0
				cfg.SubmitTimeout = 2 * time.Second
				pool := newTestPool(t, cfg)
				t.Cleanup(func() { _ = pool.stop(context.Background()) })

				release := make(chan struct{})
				require.NoError(t, pool.Spawn(context.Background(), "occupying", func(context.Context) { <-release }))
				waitForCondition(t, func() bool { return pool.Stats().Running == 1 }, "occupying task to start")

				done := make(chan error, 1)
				go func() {
					done <- pool.Spawn(context.Background(), "waits-for-slot", func(context.Context) {})
				}()

				time.Sleep(100 * time.Millisecond)
				close(release)

				select {
				case err := <-done:
					assert.NoError(t, err)
				case <-time.After(2 * time.Second):
					t.Fatal("Spawn did not succeed once a slot freed")
				}
			})

			t.Run("SubmitTimeoutFailsAfterTimeout", func(t *testing.T) {
				cfg := baseCfg
				cfg.MaxConcurrency = 1
				cfg.QueueCapacity = 0
				cfg.SubmitTimeout = 100 * time.Millisecond
				pool := newTestPool(t, cfg)
				t.Cleanup(func() { _ = pool.stop(context.Background()) })

				release := make(chan struct{})
				defer close(release)
				require.NoError(t, pool.Spawn(context.Background(), "occupying", func(context.Context) { <-release }))
				waitForCondition(t, func() bool { return pool.Stats().Running == 1 }, "occupying task to start")

				start := time.Now()
				err := pool.Spawn(context.Background(), "times-out", func(context.Context) {})
				elapsed := time.Since(start)
				assert.ErrorIs(t, err, ErrSaturated)
				assert.GreaterOrEqual(t, elapsed, 90*time.Millisecond)
			})

			t.Run("PanicInTaskIsRecoveredAndPoolKeepsWorking", func(t *testing.T) {
				cfg := baseCfg
				pool := newTestPool(t, cfg)
				t.Cleanup(func() { _ = pool.stop(context.Background()) })

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
			})

			t.Run("DrainWaitsForRunningAndQueuedTasksUnderLiveContext", func(t *testing.T) {
				cfg := baseCfg
				cfg.MaxConcurrency = 1
				cfg.QueueCapacity = 5
				pool := mustNewPool(t, cfg, nil)
				pool.open()

				running := newBlockingTask()
				queued := newBlockingTask()
				require.NoError(t, pool.Spawn(context.Background(), "running", running.run))
				waitForCondition(t, func() bool { return pool.Stats().Running == 1 }, "running task to start")
				require.NoError(t, pool.Spawn(context.Background(), "queued", queued.run))
				waitForCondition(t, func() bool { return pool.Stats().Queued == 1 }, "second task to queue")

				drainDone := make(chan error, 1)
				go func() { drainDone <- pool.drain(context.Background()) }()

				waitForCondition(t, func() bool {
					return pool.Spawn(context.Background(), "late", func(context.Context) {}) != nil
				}, "spawn to be refused during drain")

				close(running.release)
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

				require.NoError(t, pool.stop(context.Background()))
			})

			t.Run("ExpiredDrainLeavesTasksRunningAndStopThenCancelsThem", func(t *testing.T) {
				cfg := baseCfg
				cfg.MaxConcurrency = 1
				pool := newTestPool(t, cfg)

				running := newBlockingTask()
				require.NoError(t, pool.Spawn(context.Background(), "running", running.run))
				waitForCondition(t, func() bool { return pool.Stats().Running == 1 }, "running task to start")

				drainCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				defer cancel()
				err := pool.drain(drainCtx)
				assert.ErrorIs(t, err, context.DeadlineExceeded)

				select {
				case <-running.finished:
					t.Fatal("drain must not cancel work that is still running")
				case <-time.After(50 * time.Millisecond):
				}

				stopErr := pool.stop(context.Background())
				assert.NoError(t, stopErr)

				select {
				case taskErr := <-running.finished:
					assert.ErrorIs(t, taskErr, context.Canceled, "Stop must cancel the task the drain left running")
				case <-time.After(2 * time.Second):
					t.Fatal("Stop did not cancel the running task")
				}
			})

			t.Run("StopDiscardsQueuedTasks", func(t *testing.T) {
				cfg := baseCfg
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
			})

			t.Run("GlobalBindingRoutesSpawnToThisPool", func(t *testing.T) {
				resetGlobal(t)
				cfg := baseCfg
				pool := mustNewPool(t, cfg, nil)
				require.NoError(t, initPool(pool, runtimeContext(newFakeHost(), "")))
				t.Cleanup(func() { _ = pool.stop(context.Background()) })

				done := make(chan struct{})
				err := Spawn(context.Background(), "global-task", func(context.Context) { close(done) })
				require.NoError(t, err)
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("globally spawned task did not run")
				}
			})
		})
	}
}
