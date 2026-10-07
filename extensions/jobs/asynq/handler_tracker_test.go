package asynq

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestHandlerTrackerWaitsForEveryBusyPeriod pins the tracking drain relies on:
// a wait blocks while a handler is in flight and returns once it ended, and a
// handler that starts after an earlier one ended re-arms the wait instead of
// reusing the previous idle observation.
func TestHandlerTrackerWaitsForEveryBusyPeriod(t *testing.T) {
	tracker := newHandlerTracker()
	if err := tracker.waitIdle(context.Background()); err != nil {
		t.Fatalf("waitIdle() on an idle tracker error = %v", err)
	}

	tracker.begin()
	waiting := make(chan error, 1)
	go func() { waiting <- tracker.waitIdle(context.Background()) }()
	select {
	case err := <-waiting:
		t.Fatalf("waitIdle returned while a handler was in flight: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	tracker.end()

	tracker.begin()
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tracker.waitIdle(expired); !errors.Is(err, context.Canceled) {
		t.Fatal("waitIdle observed an earlier idle period while a handler was in flight")
	}
	tracker.end()
	if err := <-waiting; err != nil {
		t.Fatalf("waitIdle() error = %v", err)
	}
}
