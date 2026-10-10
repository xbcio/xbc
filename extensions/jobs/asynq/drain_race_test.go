package asynq

import (
	"context"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	hibiken "github.com/hibiken/asynq"
	goredis "github.com/redis/go-redis/v9"
)

// TestConcurrentDrainsReplayTheWorkerStopFailure runs many concurrent Drain
// callers against a worker whose Stop call panics. The worker is stopped
// exactly once and every caller returns the same cached failure, so drain is
// idempotent including its result.
func TestConcurrentDrainsReplayTheWorkerStopFailure(t *testing.T) {
	redisServer := miniredis.RunT(t)
	for iteration := 0; iteration < 50; iteration++ {
		worker := &fakeWorkerServer{stopPanic: "boom"}
		p := newTestPlugin(t, validTestConfig(redisServer.Addr()), HandlerFunc(func(context.Context, Task) error { return nil }))
		p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer { return worker }
		host := newTestHost()
		if err := p.init(testContext(host)); err != nil {
			t.Fatalf("iteration %d: init() error = %v", iteration, err)
		}

		const callers = 8
		var barrier sync.WaitGroup
		barrier.Add(callers)
		results := make(chan error, callers)
		for i := 0; i < callers; i++ {
			go func() {
				barrier.Done()
				barrier.Wait()
				results <- p.drain(context.Background())
			}()
		}

		first := <-results
		if first == nil {
			t.Fatalf("iteration %d: a drain() caller error = nil, want the worker Stop panic", iteration)
		}
		for i := 1; i < callers; i++ {
			if err := <-results; err == nil || err.Error() != first.Error() {
				t.Fatalf("iteration %d: drain() caller %d error = %v, want the cached %v", iteration, i, err, first)
			}
		}
		if worker.stopCount() != 1 {
			t.Fatalf("iteration %d: worker Stop count = %d, want 1", iteration, worker.stopCount())
		}
		host.stopTasks()
		// Init installs the process-wide remote task executor and only stop
		// uninstalls it, so every iteration ends with a stop: the first
		// iteration's executor would otherwise outlive the miniredis it
		// points at and take over the slot for every later test in the binary.
		if err := p.stop(context.Background()); err != nil {
			t.Fatalf("iteration %d: stop() error = %v", iteration, err)
		}
	}
}

// TestDrainAndStopReportTheirOwnFailures pins that the two halves own
// independent failures, each replayed to later callers of the same half: a
// worker whose Stop panics fails drain, a worker whose Shutdown panics fails
// stop, and neither hides the other.
func TestDrainAndStopReportTheirOwnFailures(t *testing.T) {
	redisServer := miniredis.RunT(t)
	worker := &fakeWorkerServer{stopPanic: "stop boom", shutdownPanic: "shutdown boom"}
	p := newTestPlugin(t, validTestConfig(redisServer.Addr()), HandlerFunc(func(context.Context, Task) error { return nil }))
	p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer { return worker }
	host := newTestHost()
	defer host.stopTasks()
	if err := p.init(testContext(host)); err != nil {
		t.Fatal(err)
	}

	first := p.drain(context.Background())
	if first == nil {
		t.Fatal("drain() error = nil, want the worker Stop panic")
	}
	second := p.drain(context.Background())
	if second == nil || second.Error() != first.Error() {
		t.Fatalf("repeated drain() error = %v, want the cached %v", second, first)
	}
	if err := p.stop(context.Background()); err == nil {
		t.Fatal("stop() error = nil, want the worker Shutdown panic")
	}
	if err := p.stop(context.Background()); err == nil {
		t.Fatal("repeated stop() error = nil, want the cached Shutdown panic")
	}
}

// TestConcurrentDrainAndStopBothComplete races the two shutdown halves in both
// call orders. Stop never waits for drain, so neither side can keep the other
// from finishing, and the worker is shut down exactly once.
func TestConcurrentDrainAndStopBothComplete(t *testing.T) {
	redisServer := miniredis.RunT(t)
	for iteration := 0; iteration < 100; iteration++ {
		worker := &fakeWorkerServer{}
		p := newTestPlugin(t, validTestConfig(redisServer.Addr()), HandlerFunc(func(context.Context, Task) error { return nil }))
		p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer { return worker }
		host := newTestHost()
		if err := p.init(testContext(host)); err != nil {
			t.Fatalf("iteration %d: init() error = %v", iteration, err)
		}

		var barrier sync.WaitGroup
		barrier.Add(2)
		results := make(chan error, 2)
		runDrain := func() {
			barrier.Done()
			barrier.Wait()
			results <- p.drain(context.Background())
		}
		runStop := func() {
			barrier.Done()
			barrier.Wait()
			results <- p.stop(context.Background())
		}
		// Alternate call order across iterations so neither goroutine is
		// reliably scheduled first.
		if iteration%2 == 0 {
			go runDrain()
			go runStop()
		} else {
			go runStop()
			go runDrain()
		}

		if err := <-results; err != nil {
			t.Fatalf("iteration %d: shutdown half error = %v, want nil", iteration, err)
		}
		if err := <-results; err != nil {
			t.Fatalf("iteration %d: shutdown half error = %v, want nil", iteration, err)
		}
		host.stopTasks()

		if worker.shutdownCount() != 1 {
			t.Fatalf("iteration %d: worker Shutdown count = %d, want 1", iteration, worker.shutdownCount())
		}
		if stops := worker.stopCount(); stops > 1 {
			t.Fatalf("iteration %d: worker Stop count = %d, want at most 1", iteration, stops)
		}
	}
}

// TestStopWithoutDrainReportsTheFailure pins the never-drained path: with no
// Drain call at all, stop still shuts the worker down and is the one to report
// the Shutdown failure.
func TestStopWithoutDrainReportsTheFailure(t *testing.T) {
	redisServer := miniredis.RunT(t)
	worker := &fakeWorkerServer{shutdownPanic: "boom"}
	p := newTestPlugin(t, validTestConfig(redisServer.Addr()), HandlerFunc(func(context.Context, Task) error { return nil }))
	p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer { return worker }
	host := newTestHost()
	defer host.stopTasks()
	if err := p.init(testContext(host)); err != nil {
		t.Fatal(err)
	}

	if err := p.stop(context.Background()); err == nil {
		t.Fatal("stop() error = nil, want the Shutdown panic")
	}
	if worker.shutdownCount() != 1 {
		t.Fatalf("worker Shutdown count = %d, want 1", worker.shutdownCount())
	}
}
