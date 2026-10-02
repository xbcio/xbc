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
// failure must appear exactly once: claimDrainErr serializes the read and the
// claim in one critical section, so whichever call wakes up first is the only
// one that gets a non-nil error.
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

// TestDrainTimeoutLeavesTheFailureForStop pins the documented timeout
// behavior: when drain's context expires before Shutdown finishes, drain
// returns the deadline error and the eventual Shutdown failure belongs to the
// stop that follows, reported exactly once.
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
