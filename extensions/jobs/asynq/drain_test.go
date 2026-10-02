package asynq

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	hibiken "github.com/hibiken/asynq"
	goredis "github.com/redis/go-redis/v9"
)

// TestDrainShutsTheWorkerDownOnceAndKeepsEnqueueOpenUntilStop pins the split
// between the two shutdown halves: drain stops consumption, while enqueue and
// Redis remain usable until stop, and stop never shuts the worker down twice.
func TestDrainShutsTheWorkerDownOnceAndKeepsEnqueueOpenUntilStop(t *testing.T) {
	redisServer := miniredis.RunT(t)
	worker := &fakeWorkerServer{}
	p := newTestPlugin(t, validTestConfig(redisServer.Addr()), HandlerFunc(func(context.Context, Task) error { return nil }))
	p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer { return worker }
	host := newTestHost()
	defer host.stopTasks()
	ctx := testContext(host)
	if err := p.init(ctx); err != nil {
		t.Fatal(err)
	}
	client := p.client
	if err := p.start(ctx); err != nil {
		t.Fatal(err)
	}
	host.openTraffic()
	waitForCondition(t, time.Second, func() bool { return worker.startCount() == 1 }, "worker start after traffic gate")

	if err := p.drain(context.Background()); err != nil {
		t.Fatalf("drain() error = %v", err)
	}
	if worker.shutdownCount() != 1 {
		t.Fatalf("worker Shutdown count after drain = %d, want 1", worker.shutdownCount())
	}
	if _, err := client.Enqueue(context.Background(), Task{Type: "work"}); err != nil {
		t.Fatalf("Enqueue after drain error = %v, want enqueue to stay open until Stop", err)
	}
	if err := p.drain(context.Background()); err != nil {
		t.Fatalf("repeated drain() error = %v", err)
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
	if worker.shutdownCount() != 1 {
		t.Fatalf("worker Shutdown count after stop = %d, want 1", worker.shutdownCount())
	}
	if _, err := client.Enqueue(context.Background(), Task{Type: "work"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Enqueue after stop error = %v, want ErrClosed", err)
	}
}

// TestDrainHonorsItsDeadlineAndStopWaitsForTheSameShutdown covers a handler
// that outlives the drain budget: drain returns at its deadline, and stop
// waits for the Shutdown drain already started rather than starting another.
func TestDrainHonorsItsDeadlineAndStopWaitsForTheSameShutdown(t *testing.T) {
	redisServer := miniredis.RunT(t)
	worker := &fakeWorkerServer{shutdownStarted: make(chan struct{}), shutdownRelease: make(chan struct{})}
	p := newTestPlugin(t, validTestConfig(redisServer.Addr()), HandlerFunc(func(context.Context, Task) error { return nil }))
	p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer { return worker }
	host := newTestHost()
	defer host.stopTasks()
	if err := p.init(testContext(host)); err != nil {
		t.Fatal(err)
	}

	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.drain(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain() error = %v, want deadline", err)
	}
	if err := p.start(testContext(host)); err == nil {
		t.Fatal("start() after drain error = nil")
	}

	stopped := make(chan error, 1)
	go func() { stopped <- p.stop(context.Background()) }()
	select {
	case err := <-stopped:
		t.Fatalf("stop returned before the drain's Shutdown finished: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(worker.shutdownRelease)
	if err := <-stopped; err != nil {
		t.Fatalf("stop() error = %v", err)
	}
	if worker.shutdownCount() != 1 {
		t.Fatalf("worker Shutdown count = %d, want 1", worker.shutdownCount())
	}
}

// TestDrainFailureIsReportedOnceAndStopReportsAnUnobservedOne pins that a
// Shutdown failure is reported by whichever stage observed it, never by both.
func TestDrainFailureIsReportedOnceAndStopReportsAnUnobservedOne(t *testing.T) {
	redisServer := miniredis.RunT(t)
	for name, observed := range map[string]bool{"observed by drain": true, "left for stop": false} {
		t.Run(name, func(t *testing.T) {
			worker := &fakeWorkerServer{shutdownPanic: "boom"}
			if !observed {
				worker.shutdownStarted = make(chan struct{})
				worker.shutdownRelease = make(chan struct{})
			}
			p := newTestPlugin(t, validTestConfig(redisServer.Addr()), HandlerFunc(func(context.Context, Task) error { return nil }))
			p.factory.newServer = func(goredis.UniversalClient, hibiken.Config) workerServer { return worker }
			host := newTestHost()
			defer host.stopTasks()
			if err := p.init(testContext(host)); err != nil {
				t.Fatal(err)
			}
			if observed {
				if err := p.drain(context.Background()); err == nil {
					t.Fatal("drain() error = nil, want the Shutdown panic")
				}
				if err := p.stop(context.Background()); err != nil {
					t.Fatalf("stop() error = %v, want the drain's failure not repeated", err)
				}
				return
			}
			short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			if err := p.drain(short); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("drain() error = %v, want deadline", err)
			}
			close(worker.shutdownRelease)
			if err := p.stop(context.Background()); err == nil {
				t.Fatal("stop() error = nil, want the Shutdown panic drain never observed")
			}
		})
	}
}
