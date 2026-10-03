package async

import (
	"context"
	"sync"
	"testing"
)

// BenchmarkSpawn measures parallel Spawn throughput for a tiny task against
// both executors, sized so admission is never the bottleneck
// (MaxConcurrency comfortably exceeds GOMAXPROCS * b.N's typical
// parallelism): the number being compared is each executor's own per-task
// overhead (goroutine creation and teardown vs. ants' pooled worker reuse),
// not queueing or saturation behavior.
//
// Run with: go test -run '^$' -bench Spawn -benchmem -count=5
func BenchmarkSpawn(b *testing.B) {
	for name, cfg := range executorConfigs() {
		cfg := cfg
		cfg.MaxConcurrency = 1024
		cfg.QueueCapacity = 1 << 20 // large enough that this benchmark never saturates
		b.Run(name, func(b *testing.B) {
			pool, err := newPreparedPool(cfg, nil)
			if err != nil {
				b.Fatalf("newPreparedPool: %v", err)
			}
			b.Cleanup(func() { _ = pool.stop(context.Background()) })

			ctx := context.Background()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				var wg sync.WaitGroup
				for pb.Next() {
					wg.Add(1)
					if spawnErr := pool.Spawn(ctx, "bench", func(context.Context) { wg.Done() }); spawnErr != nil {
						wg.Done()
						b.Error(spawnErr)
					}
				}
				wg.Wait()
			})
		})
	}
}

// BenchmarkSpawnQueued measures Spawn throughput under a saturated,
// queueing Pool: MaxConcurrency is fixed at 8 (deliberately small relative
// to typical parallelism) and QueueCapacity is large enough that a burst of
// concurrent Spawn calls mostly lands in the queue rather than a running
// slot. Unlike BenchmarkSpawn (which keeps admission off the critical
// path), this exercises the worker loop itself: most tasks are picked up by
// nextOrRelease inside runWorker rather than freshly submitted to the
// executor, which is the path the worker-loop change (replacing a
// goroutine-per-queued-task dispatch with the finishing worker looping on
// its own queue) is meant to speed up.
//
// Run with: go test -run '^$' -bench Spawn -benchmem -count=5
func BenchmarkSpawnQueued(b *testing.B) {
	for name, cfg := range executorConfigs() {
		cfg := cfg
		cfg.MaxConcurrency = 8
		cfg.QueueCapacity = 1 << 20 // large enough that a burst always queues rather than ever hitting ErrSaturated
		cfg.SubmitTimeout = 0
		b.Run(name, func(b *testing.B) {
			pool, err := newPreparedPool(cfg, nil)
			if err != nil {
				b.Fatalf("newPreparedPool: %v", err)
			}
			b.Cleanup(func() { _ = pool.stop(context.Background()) })

			ctx := context.Background()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				var wg sync.WaitGroup
				for pb.Next() {
					wg.Add(1)
					if spawnErr := pool.Spawn(ctx, "bench", func(context.Context) { wg.Done() }); spawnErr != nil {
						wg.Done()
						b.Error(spawnErr)
					}
				}
				wg.Wait()
			})
		})
	}
}
