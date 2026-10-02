package auditlog

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestDrainWritesAcceptedEventsAndLeavesTheFlushToStop pins the split between
// the two shutdown halves: Drain empties the queue into the sink, and Stop is
// what flushes it, exactly once.
func TestDrainWritesAcceptedEventsAndLeavesTheFlushToStop(t *testing.T) {
	sink := &memorySink{}
	cfg := DefaultConfig()
	cfg.Async = true
	cfg.QueueSize = 32
	p, err := New(cfg, WithSink(sink))
	if err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{}
	if err := p.Start(runtimeContext(host)); err != nil {
		t.Fatal(err)
	}
	state := p.state.Load()
	for i := range 10 {
		if err := state.dispatch.submit(context.Background(), Event{Status: 200 + i}); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Drain(context.Background()); err != nil {
		t.Fatalf("Drain() error = %v", err)
	}
	events, flushes := sink.snapshot()
	if len(events) != 10 || flushes != 0 {
		t.Fatalf("after Drain events=%d flushes=%d, want 10/0", len(events), flushes)
	}
	if err := state.dispatch.submit(context.Background(), Event{Status: 299}); !errors.Is(err, ErrStopped) {
		t.Fatalf("submit after Drain error = %v, want ErrStopped", err)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	host.wg.Wait()
	if events, flushes = sink.snapshot(); len(events) != 10 || flushes != 1 {
		t.Fatalf("after Stop events=%d flushes=%d, want 10/1", len(events), flushes)
	}
}

// TestExpiredDrainKeepsWritingForTheStopThatFollows covers a sink slower than
// the drain budget: the expired Drain must not abandon the queued events, so
// the Stop that follows still writes them.
func TestExpiredDrainKeepsWritingForTheStopThatFollows(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	sink := &memorySink{block: gate, entered: entered}
	cfg := DefaultConfig()
	cfg.Async = true
	cfg.QueueSize = 4
	cfg.SinkTimeout = 5 * time.Second
	p, err := New(cfg, WithSink(sink))
	if err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{}
	if err := p.Start(runtimeContext(host)); err != nil {
		t.Fatal(err)
	}
	state := p.state.Load()
	for i := range 2 {
		if err := state.dispatch.submit(context.Background(), Event{Status: 200 + i}); err != nil {
			t.Fatal(err)
		}
	}
	<-entered

	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.Drain(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain() error = %v, want deadline while the sink blocks", err)
	}
	close(gate)
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	host.wg.Wait()
	if events, _ := sink.snapshot(); len(events) != 2 {
		t.Fatalf("events written = %d, want both accepted events despite the expired Drain", len(events))
	}
}

// TestDrainIsANoOpForSynchronousDispatchAndBeforeStart covers the two shapes
// with nothing queued to wait for.
func TestDrainIsANoOpForSynchronousDispatchAndBeforeStart(t *testing.T) {
	synchronous, err := New(DefaultConfig(), WithSink(&memorySink{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := synchronous.Drain(context.Background()); err != nil {
		t.Fatalf("synchronous Drain() error = %v", err)
	}

	sink := &memorySink{}
	cfg := DefaultConfig()
	cfg.Async = true
	unstarted, err := New(cfg, WithSink(sink))
	if err != nil {
		t.Fatal(err)
	}
	if err := unstarted.state.Load().dispatch.submit(context.Background(), Event{Status: 200}); err != nil {
		t.Fatal(err)
	}
	if err := unstarted.Drain(context.Background()); err != nil {
		t.Fatalf("Drain() before Start error = %v", err)
	}
	if events, _ := sink.snapshot(); len(events) != 1 {
		t.Fatalf("events written = %d, want the queued event written without a worker", len(events))
	}
	if err := unstarted.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}
