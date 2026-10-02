package asynq

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	hibiken "github.com/hibiken/asynq"
	goredis "github.com/redis/go-redis/v9"
)

// TestConcurrentDrainAndStopReportAFailureExactlyOnce forces drain and stop to
// observe the worker's Shutdown panic at the same time, in both call orders,
// across many iterations. Across all the errors returned by both calls, the
// failure must appear exactly once: claimDrainErrForDrain and
// claimDrainErrForStop serialize the read and the claim in one critical
// section, so whichever side wakes up first is the only one that owns the
// failure.
func TestConcurrentDrainAndStopReportAFailureExactlyOnce(t *testing.T) {
	redisServer := miniredis.RunT(t)
	for iteration := 0; iteration < 200; iteration++ {
		worker := &fakeWorkerServer{shutdownPanic: "boom"}
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

		first := <-results
		second := <-results
		host.stopTasks()

		reported := 0
		if first != nil {
			reported++
		}
		if second != nil {
			reported++
		}
		if reported != 1 {
			t.Fatalf("iteration %d: failure reported %d times (drain/stop errors = %v, %v), want exactly once", iteration, reported, first, second)
		}
	}
}

// TestSequentialDrainThenStopReportsOnlyFromDrain pins the ordinary ordering:
// a Shutdown failure observed by drain is not repeated by a later stop.
func TestSequentialDrainThenStopReportsOnlyFromDrain(t *testing.T) {
	redisServer := miniredis.RunT(t)
	worker := &fakeWorkerServer{shutdownPanic: "boom"}
	p := newTestPlugin(t, validTestConfig(redisServer.Addr()), HandlerFunc(func(context.Context, Task) error { return nil }))
	p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer { return worker }
	host := newTestHost()
	defer host.stopTasks()
	if err := p.init(testContext(host)); err != nil {
		t.Fatal(err)
	}

	if err := p.drain(context.Background()); err == nil {
		t.Fatal("drain() error = nil, want the Shutdown panic")
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v, want nil because drain already reported the failure", err)
	}
}

// TestSequentialDrainTwiceBothReturnTheFailure pins Drain's idempotence: a
// second Drain call, with no Stop involved, must still return the same
// failure the first Drain call returned instead of losing it to nil.
func TestSequentialDrainTwiceBothReturnTheFailure(t *testing.T) {
	redisServer := miniredis.RunT(t)
	worker := &fakeWorkerServer{shutdownPanic: "boom"}
	p := newTestPlugin(t, validTestConfig(redisServer.Addr()), HandlerFunc(func(context.Context, Task) error { return nil }))
	p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer { return worker }
	host := newTestHost()
	defer host.stopTasks()
	if err := p.init(testContext(host)); err != nil {
		t.Fatal(err)
	}

	first := p.drain(context.Background())
	if first == nil {
		t.Fatal("first drain() error = nil, want the Shutdown panic")
	}
	second := p.drain(context.Background())
	if second == nil {
		t.Fatalf("second drain() error = nil, want the same failure as the first drain() error = %v", first)
	}
	if first.Error() != second.Error() {
		t.Fatalf("second drain() error = %v, want the same failure as the first drain() error = %v", second, first)
	}
}

// TestConcurrentDrainsAllReturnTheFailure runs many concurrent Drain callers
// against the same completion and checks every one of them observes and
// returns the cached failure, not just the first.
func TestConcurrentDrainsAllReturnTheFailure(t *testing.T) {
	redisServer := miniredis.RunT(t)
	for iteration := 0; iteration < 20; iteration++ {
		worker := &fakeWorkerServer{shutdownPanic: "boom"}
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

		for i := 0; i < callers; i++ {
			if err := <-results; err == nil {
				t.Fatalf("iteration %d: drain() caller %d error = nil, want the Shutdown panic", iteration, i)
			}
		}
		host.stopTasks()
	}
}

// TestConcurrentDrainsAndStopOnlyDrainReportsTheFailure runs many concurrent
// Drain callers alongside a single Stop call. Every Drain caller that
// observes completion must return the failure, and Stop must never return it
// (only its own unrelated errors, of which there are none here).
func TestConcurrentDrainsAndStopOnlyDrainReportsTheFailure(t *testing.T) {
	redisServer := miniredis.RunT(t)
	for iteration := 0; iteration < 20; iteration++ {
		worker := &fakeWorkerServer{shutdownPanic: "boom"}
		p := newTestPlugin(t, validTestConfig(redisServer.Addr()), HandlerFunc(func(context.Context, Task) error { return nil }))
		p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer { return worker }
		host := newTestHost()
		if err := p.init(testContext(host)); err != nil {
			t.Fatalf("iteration %d: init() error = %v", iteration, err)
		}

		const drainers = 8
		var barrier sync.WaitGroup
		barrier.Add(drainers)
		drainResults := make(chan error, drainers)
		for i := 0; i < drainers; i++ {
			go func() {
				barrier.Done()
				barrier.Wait()
				drainResults <- p.drain(context.Background())
			}()
		}

		// Wait for at least one drain caller to observe completion before
		// starting stop, so stop is guaranteed to race against an ownership
		// decision the drain side has already made, rather than racing to
		// make that decision itself.
		observedFailure := false
		remaining := drainers
		first := <-drainResults
		remaining--
		if first != nil {
			observedFailure = true
		}

		stopResult := make(chan error, 1)
		go func() { stopResult <- p.stop(context.Background()) }()

		for ; remaining > 0; remaining-- {
			if err := <-drainResults; err != nil {
				observedFailure = true
			}
		}
		stopErr := <-stopResult
		host.stopTasks()

		if !observedFailure {
			t.Fatalf("iteration %d: no drain() caller observed the Shutdown panic", iteration)
		}
		if stopErr != nil {
			t.Fatalf("iteration %d: stop() error = %v, want nil because a drain() caller observed the failure", iteration, stopErr)
		}
	}
}

// TestDrainTimeoutLeavesTheFailureForStop pins the documented timeout
// behavior: when drain's context expires before Shutdown finishes, drain
// returns the deadline error and the eventual Shutdown failure belongs to the
// stop that follows, reported exactly once. A drain call made after stop has
// claimed the failure does not re-report it.
func TestDrainTimeoutLeavesTheFailureForStop(t *testing.T) {
	redisServer := miniredis.RunT(t)
	worker := &fakeWorkerServer{
		shutdownPanic:   "boom",
		shutdownStarted: make(chan struct{}),
		shutdownRelease: make(chan struct{}),
	}
	p := newTestPlugin(t, validTestConfig(redisServer.Addr()), HandlerFunc(func(context.Context, Task) error { return nil }))
	p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer { return worker }
	host := newTestHost()
	defer host.stopTasks()
	if err := p.init(testContext(host)); err != nil {
		t.Fatal(err)
	}

	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := p.drain(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain() error = %v, want deadline", err)
	}

	close(worker.shutdownRelease)
	if err := p.stop(context.Background()); err == nil {
		t.Fatal("stop() error = nil, want the Shutdown panic that drain never observed")
	}

	// A second stop must not repeat the already-reported failure; stop is
	// idempotent and returns the same cached result.
	if err := p.stop(context.Background()); err == nil {
		t.Fatal("repeated stop() error = nil, want the cached failure returned again, not re-derived")
	}

	// A drain call made after stop has claimed the failure must not
	// re-report it: stop is already the documented sole owner once it claims
	// a failure no drain call observed in time.
	if err := p.drain(context.Background()); err != nil {
		t.Fatalf("drain() after stop error = %v, want nil because stop already reported the failure", err)
	}
}

// TestStopWithoutDrainReportsTheFailure pins the never-drained path: with no
// Drain call at all, stop is the sole owner of the Shutdown failure.
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
}
