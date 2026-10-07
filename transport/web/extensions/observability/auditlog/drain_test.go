package auditlog

import (
	"context"
	"errors"
	"sync"
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
	if err := unstarted.Start(runtimeContext(&fakeHost{})); err == nil {
		t.Fatal("Start() after Drain error = nil")
	}
	if err := synchronous.Start(runtimeContext(&fakeHost{})); err == nil {
		t.Fatal("synchronous Start() after Drain error = nil")
	}
	if err := unstarted.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestStopAfterExpiredDrainUsesItsOwnDeadlineAndFlushes pins the shutdown
// budget split: an expired Drain only stops waiting, and the Stop that follows
// drains the rest of the queue under its own (ample) ctx rather than under
// sink_timeout, then still flushes the sink.
func TestStopAfterExpiredDrainUsesItsOwnDeadlineAndFlushes(t *testing.T) {
	sink := &delayedSink{delay: 40 * time.Millisecond}
	cfg := DefaultConfig()
	cfg.Async = true
	cfg.QueueSize = 8
	cfg.SinkTimeout = 100 * time.Millisecond
	p, err := New(cfg, WithSink(sink))
	if err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{}
	if err := p.Start(runtimeContext(host)); err != nil {
		t.Fatal(err)
	}
	state := p.state.Load()
	const events = 6
	for i := range events {
		if err := state.dispatch.submit(context.Background(), Event{Status: 200 + i}); err != nil {
			t.Fatal(err)
		}
	}

	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := p.Drain(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain() error = %v, want deadline while the sink is slow", err)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	host.wg.Wait()
	written, flushes := sink.snapshot()
	if len(written) != events {
		t.Fatalf("events written = %d, want all %d accepted events", len(written), events)
	}
	if flushes != 1 {
		t.Fatalf("flushes = %d, want the sink flushed once", flushes)
	}
}

// TestStopAbandoningTheQueueStillFlushes pins that Stop never skips the flush
// when the remaining queue has to be abandoned, and that the attempt itself is
// bounded by the caller's ctx: the abandoned worker is cancelled rather than
// awaited, and the flush runs on the deadline the drain already spent, so the
// sink refuses it instead of the flush buying back a fresh budget.
func TestStopAbandoningTheQueueStillFlushes(t *testing.T) {
	gate := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	// A t.Fatal below ends the test body before the worker is released, and
	// the parked goroutine must not be left waiting on a gate nobody closes.
	t.Cleanup(release)
	entered := make(chan struct{}, 1)
	sink := &attemptSink{memorySink: memorySink{block: gate, entered: entered}}
	cfg := DefaultConfig()
	cfg.Async = true
	cfg.QueueSize = 4
	cfg.SinkTimeout = time.Second
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

	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := p.Drain(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain() error = %v, want deadline while the sink blocks", err)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stopCancel()
	if err := p.Stop(stopCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop() error = %v, want deadline once the queue is abandoned", err)
	}
	// The worker was cancelled, not awaited, and exits as soon as the write it
	// is parked in honors the Sink contract's cancellation requirement.
	release()
	host.wg.Wait()
	select {
	case <-state.dispatch.done:
	default:
		t.Fatal("the cancelled dispatcher worker did not exit")
	}
	// The flush was attempted on the caller's ctx, which the drain had already
	// spent: the attempt must be made, and the sink's refusal of the cancelled
	// context is what keeps Stop inside the caller's deadline.
	attempts := sink.flushAttempts()
	if _, flushes := sink.snapshot(); attempts != 1 || flushes != 0 {
		t.Fatalf("flush attempts=%d flushes=%d, want the attempt refused on the spent deadline", attempts, flushes)
	}
}

// TestStopReturnsByTheCallerDeadlineWhenTheSinkBlocks pins the bound the caller
// buys with its ctx: a sink parked in a write that ignores cancellation must
// not hold Stop open. Stop cancels the abandoned worker and leaves it to unwind
// rather than waiting for it, so the shutdown budget stays the caller's and no
// sink can extend it.
func TestStopReturnsByTheCallerDeadlineWhenTheSinkBlocks(t *testing.T) {
	gate := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	// A failure below must not leave the parked worker waiting on a gate
	// nobody closes.
	t.Cleanup(release)
	entered := make(chan struct{}, 1)
	sink := &stubbornSink{gate: gate, entered: entered}
	cfg := DefaultConfig()
	cfg.Async = true
	cfg.QueueSize = 4
	// Deliberately far larger than the caller's deadline: a Stop that let the
	// sink's write budget, or a flush budget detached from the caller, set the
	// pace would still be running when the assertion below fires.
	cfg.SinkTimeout = 5 * time.Second
	p, err := New(cfg, WithSink(sink))
	if err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{}
	if err := p.Start(runtimeContext(host)); err != nil {
		t.Fatal(err)
	}
	if err := p.state.Load().dispatch.submit(context.Background(), Event{Status: 200}); err != nil {
		t.Fatal(err)
	}
	<-entered

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stopCancel()
	started := time.Now()
	stopped := make(chan error, 1)
	go func() { stopped <- p.Stop(stopCtx) }()
	var stopErr error
	select {
	case stopErr = <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return once the caller's deadline expired")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Stop took %s, want it bounded by the caller's deadline", elapsed)
	}
	if !errors.Is(stopErr, context.DeadlineExceeded) {
		t.Fatalf("Stop() error = %v, want deadline once the sink ignored it", stopErr)
	}
	release()
	host.wg.Wait()
}

// delayedSink stands in for a sink slower than the configured sink_timeout
// budget by delaying every write.
type delayedSink struct {
	memorySink
	delay time.Duration
}

func (s *delayedSink) Write(ctx context.Context, event Event) error {
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.memorySink.Write(ctx, event)
}

// attemptSink counts every Flush call separately from the flushes that ran, so
// a test can tell a flush Stop skipped from one it attempted on an already
// spent deadline and the sink refused.
type attemptSink struct {
	memorySink

	mu       sync.Mutex
	attempts int
}

func (s *attemptSink) Flush(ctx context.Context) error {
	s.mu.Lock()
	s.attempts++
	s.mu.Unlock()
	return s.memorySink.Flush(ctx)
}

func (s *attemptSink) flushAttempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

// stubbornSink violates the Sink contract on purpose: Write parks until it is
// released and ignores the context, standing in for a sink that refuses
// cancellation, so a Stop that waits for the worker rather than cancelling it
// cannot return on its own.
type stubbornSink struct {
	gate    <-chan struct{}
	entered chan<- struct{}
}

func (s *stubbornSink) Write(context.Context, Event) error {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-s.gate
	return nil
}

// TestConcurrentDrainAndStopWithoutAWorkerWriteEachEventOnce covers the
// never-started dispatcher, where Drain and Stop both write the queue from the
// caller's goroutine: overlapping calls must neither lose nor duplicate an
// event, and the sink is flushed exactly once.
func TestConcurrentDrainAndStopWithoutAWorkerWriteEachEventOnce(t *testing.T) {
	sink := &memorySink{}
	cfg := DefaultConfig()
	cfg.Async = true
	cfg.QueueSize = 32
	p, err := New(cfg, WithSink(sink))
	if err != nil {
		t.Fatal(err)
	}
	state := p.state.Load()
	for i := range 20 {
		if err := state.dispatch.submit(context.Background(), Event{Status: 200 + i}); err != nil {
			t.Fatal(err)
		}
	}
	errs := make(chan error, 4)
	for i := range 4 {
		go func() {
			if i%2 == 0 {
				errs <- p.Drain(context.Background())
				return
			}
			errs <- p.Stop(context.Background())
		}()
	}
	for range 4 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	events, flushes := sink.snapshot()
	if len(events) != 20 || flushes != 1 {
		t.Fatalf("events=%d flushes=%d, want 20/1", len(events), flushes)
	}
	seen := make(map[int]bool, len(events))
	for _, event := range events {
		if seen[event.Status] {
			t.Fatalf("event %d written twice", event.Status)
		}
		seen[event.Status] = true
	}
}
