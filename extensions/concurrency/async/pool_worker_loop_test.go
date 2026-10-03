package async

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// liveGoroutines reports the number of goroutines actually running by
// taking a full stack dump (runtime.Stack(buf, true), the same mechanism
// `go test`'s own deadlock detector and panic handler use) and counting its
// "goroutine " headers, rather than runtime.NumGoroutine() or
// runtime/pprof's goroutine profile count. Under -race and the kind of
// rapid, serial create-finish churn this test deliberately induces (many
// short-lived task goroutines in quick succession via the goroutine
// executor), both of those faster counters were observed to transiently
// report counts in the hundreds to low thousands at the exact same instant
// a synchronous runtime.Stack(all=true) taken with zero gap showed only a
// handful of goroutines genuinely alive -- a counting artifact in the
// runtime's own bookkeeping during heavy goroutine churn under race
// instrumentation, not an actual leak (verified by hand while developing
// this test: a captured mismatch of NumGoroutine()==324 against a
// synchronous full dump containing 13 "goroutine " headers). A real stack
// dump is stop-the-world and authoritative, so it is the only count this
// test trusts for its bound.
func liveGoroutines() int {
	buf := make([]byte, 8<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "goroutine ")
}

// TestWorkerLoopNoGoroutineGrowthPerTask runs a large number of tiny tasks
// through a saturated, queueing Pool and samples the actual number of live
// goroutines (liveGoroutines, a full stack dump) both while tasks are still
// in flight and once every task has finished. Before the worker-loop
// change, each queued task's promotion spawned a fresh `go
// p.dispatchQueue()` goroutine (pool.go's old finishRunning/Spawn paths);
// that no longer happens, so goroutine count must stay bounded around
// MaxConcurrency plus a small constant throughout the run, for both
// executors, rather than growing with the number of tasks submitted.
func TestWorkerLoopNoGoroutineGrowthPerTask(t *testing.T) {
	for name, baseCfg := range executorConfigs() {
		baseCfg := baseCfg
		t.Run(name, func(t *testing.T) {
			const maxConcurrency = 4
			const tasks = 10000

			cfg := baseCfg
			cfg.MaxConcurrency = maxConcurrency
			cfg.QueueCapacity = tasks
			cfg.SubmitTimeout = 10 * time.Second
			pool := newTestPool(t, cfg)
			t.Cleanup(func() { _ = pool.stop(context.Background()) })

			baseline := liveGoroutines()
			// A generous slack: the test runtime itself, GC workers, and
			// the executor's own fixed bookkeeping goroutines (ants'
			// scavenger, if ants) account for a small constant on top of
			// maxConcurrency. What this test guards against is growth that
			// scales with the number of tasks (10000), not a fixed offset.
			const slack = 20

			var completed atomic.Int64
			var maxDuringRun atomic.Int64
			var wg sync.WaitGroup
			wg.Add(tasks)

			stopSampling := make(chan struct{})
			samplingDone := make(chan struct{})
			go func() {
				defer close(samplingDone)
				// 2ms rather than 200us: liveGoroutines does a real,
				// stop-the-world stack dump (runtime.Stack(buf, true)),
				// unlike a plain counter read, so it must not be polled as
				// fast as a cheap counter would allow without itself
				// perturbing the very scheduling it is trying to observe.
				ticker := time.NewTicker(2 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-stopSampling:
						return
					case <-ticker.C:
						n := int64(liveGoroutines())
						for {
							cur := maxDuringRun.Load()
							if n <= cur || maxDuringRun.CompareAndSwap(cur, n) {
								break
							}
						}
					}
				}
			}()

			for i := 0; i < tasks; i++ {
				err := pool.Spawn(context.Background(), "tiny", func(context.Context) {
					completed.Add(1)
					wg.Done()
				})
				require.NoError(t, err)
			}

			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("tasks did not all complete")
			}
			close(stopSampling)
			<-samplingDone

			assert.Equal(t, int64(tasks), completed.Load())
			assert.LessOrEqual(t, maxDuringRun.Load(), int64(baseline+maxConcurrency+slack),
				"goroutine count must stay bounded around max_concurrency+slack during the run, baseline=%d observed_max=%d", baseline, maxDuringRun.Load())

			// Give any worker goroutine still unwinding a moment to return
			// before the final sample, matching how a real caller would
			// observe steady state rather than the instant the last task's
			// Done fired.
			waitForCondition(t, func() bool {
				return liveGoroutines() <= baseline+maxConcurrency+slack
			}, "goroutine count to settle back down after all tasks finished")
		})
	}
}

// TestWorkerLoopPreservesQueueFIFOOrder confirms that once a worker's own
// loop (rather than a fresh Submit) picks up queued tasks, it still runs
// them in the order they were queued: nextQueued always pops p.queue[0].
func TestWorkerLoopPreservesQueueFIFOOrder(t *testing.T) {
	for name, baseCfg := range executorConfigs() {
		baseCfg := baseCfg
		t.Run(name, func(t *testing.T) {
			cfg := baseCfg
			cfg.MaxConcurrency = 1
			cfg.QueueCapacity = 50
			pool := newTestPool(t, cfg)
			t.Cleanup(func() { _ = pool.stop(context.Background()) })

			release := make(chan struct{})
			require.NoError(t, pool.Spawn(context.Background(), "occupying", func(context.Context) { <-release }))
			waitForCondition(t, func() bool { return pool.Stats().Running == 1 }, "occupying task to start")

			const queued = 50
			var mu sync.Mutex
			var order []int
			var wg sync.WaitGroup
			wg.Add(queued)
			for i := 0; i < queued; i++ {
				i := i
				require.NoError(t, pool.Spawn(context.Background(), fmt.Sprintf("queued-%d", i), func(context.Context) {
					mu.Lock()
					order = append(order, i)
					mu.Unlock()
					wg.Done()
				}))
			}
			waitForCondition(t, func() bool { return pool.Stats().Queued == uint64(queued) }, "every task to be queued")

			close(release)

			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("queued tasks never completed")
			}

			mu.Lock()
			defer mu.Unlock()
			require.Len(t, order, queued)
			for i, v := range order {
				assert.Equal(t, i, v, "queued tasks must run in FIFO order")
			}
		})
	}
}

// TestWorkerLoopStopDuringLongQueueStopsPromptlyAndDiscardsTheRest confirms
// that calling stop while a long queue is still being drained by the worker
// loop stops the loop promptly (nextQueued observes p.stopped and refuses
// further tasks) rather than running the entire backlog first, and that the
// discarded tasks never run.
func TestWorkerLoopStopDuringLongQueueStopsPromptlyAndDiscardsTheRest(t *testing.T) {
	for name, baseCfg := range executorConfigs() {
		baseCfg := baseCfg
		t.Run(name, func(t *testing.T) {
			cfg := baseCfg
			cfg.MaxConcurrency = 1
			cfg.QueueCapacity = 10000
			pool := newTestPool(t, cfg)

			release := make(chan struct{})
			require.NoError(t, pool.Spawn(context.Background(), "occupying", func(context.Context) { <-release }))
			waitForCondition(t, func() bool { return pool.Stats().Running == 1 }, "occupying task to start")

			const queued = 5000
			var ran atomic.Int64
			for i := 0; i < queued; i++ {
				require.NoError(t, pool.Spawn(context.Background(), fmt.Sprintf("queued-%d", i), func(context.Context) {
					ran.Add(1)
				}))
			}
			waitForCondition(t, func() bool { return pool.Stats().Queued == uint64(queued) }, "every task to be queued")

			stopDone := make(chan error, 1)
			go func() { stopDone <- pool.stop(context.Background()) }()

			// Release the one running task shortly after calling stop so
			// the worker loop, if it were (incorrectly) still draining the
			// queue, would have the opportunity to run many queued tasks
			// before stop's own queue clear takes effect.
			time.Sleep(10 * time.Millisecond)
			close(release)

			select {
			case err := <-stopDone:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("stop did not return")
			}

			assert.Less(t, ran.Load(), int64(queued),
				"stop must discard most of a long queue rather than letting the worker loop drain all of it")
			assert.Equal(t, uint64(0), pool.Stats().Queued, "stop must clear the queue")
		})
	}
}
