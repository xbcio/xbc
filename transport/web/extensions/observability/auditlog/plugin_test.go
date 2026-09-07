package auditlog

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

type fakeHost struct {
	wg sync.WaitGroup
}

func (*fakeHost) ExecutionContext() context.Context { return context.Background() }
func (*fakeHost) Logger() log.Logger                { return log.Nop() }
func (*fakeHost) TrafficGate() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
func (h *fakeHost) SubmitTask(_ plugin.Identity, fn func(context.Context), _ bool) bool {
	if fn == nil {
		return false
	}
	h.wg.Add(1)
	go func() { defer h.wg.Done(); fn(context.Background()) }()
	return true
}
func (*fakeHost) RequestShutdown(plugin.Identity, string) bool { return true }

func runtimeContext(host plugin.RuntimeHost) *plugin.Context {
	return plugin.NewRuntimeContext(host, plugin.Identity{Plugin: Key})
}

type memorySink struct {
	mu      sync.Mutex
	events  []Event
	flushes int
	block   <-chan struct{}
	entered chan<- struct{}
}

func (s *memorySink) Write(ctx context.Context, event Event) error {
	if s.entered != nil {
		select {
		case s.entered <- struct{}{}:
		default:
		}
	}
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
	return nil
}
func (s *memorySink) Flush(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.flushes++
	s.mu.Unlock()
	return nil
}
func (s *memorySink) snapshot() ([]Event, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...), s.flushes
}

func TestDefinitionIsCanonicalAndBundleIsStable(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero || Definition() != Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}
	first, second := Bundle(), Bundle()
	if reflect.DeepEqual(first, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("Bundle() returned different composition content")
	}
}

func TestNewAppliesDefaultsAndDefaultSink(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.QueueSize != 1024 || cfg.Overflow != OverflowDropNewest || cfg.RequestIDHeader != "X-Request-ID" {
		t.Fatalf("defaults = %#v", cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	state := p.state.Load()
	if state == nil {
		t.Fatal("state = nil")
	}
	if _, ok := state.sink.(*LogSink); !ok {
		t.Fatalf("default sink = %T", state.sink)
	}
	if state.dispatch != nil {
		t.Fatal("synchronous config unexpectedly built an async dispatcher")
	}
}

func TestAsyncStopDrainsAndFlushesIdempotently(t *testing.T) {
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
	for i := range 20 {
		if err := state.dispatch.submit(context.Background(), Event{Status: 200 + i%2}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- p.Stop(ctx) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	host.wg.Wait()
	events, flushes := sink.snapshot()
	if len(events) != 20 || flushes != 1 {
		t.Fatalf("events=%d flushes=%d", len(events), flushes)
	}
}

func TestAsyncOverflowIsExplicit(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	sink := &memorySink{block: gate, entered: entered}
	dispatcher := newAsyncDispatcher(sink, log.Nop(), 1, OverflowDropNewest, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dispatcher.run(ctx)
	if err := dispatcher.submit(ctx, Event{Status: 200}); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := dispatcher.submit(ctx, Event{Status: 201}); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.submit(ctx, Event{Status: 202}); err != ErrQueueFull {
		t.Fatalf("overflow = %v", err)
	}
	if dispatcher.droppedCount() != 1 {
		t.Fatalf("dropped = %d", dispatcher.droppedCount())
	}
	close(gate)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := dispatcher.stopAndWait(stopCtx, true); err != nil {
		t.Fatal(err)
	}
}

func TestBlockOverflowSubmitIsReleasedByStop(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	sink := &memorySink{block: gate, entered: entered}
	dispatcher := newAsyncDispatcher(sink, log.Nop(), 1, OverflowBlock, time.Second)
	go dispatcher.run(context.Background())
	if err := dispatcher.submit(context.Background(), Event{Status: 200}); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := dispatcher.submit(context.Background(), Event{Status: 201}); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan error, 1)
	go func() { blocked <- dispatcher.submit(context.Background(), Event{Status: 202}) }()
	stopCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := dispatcher.stopAndWait(stopCtx, true); err == nil {
		t.Fatal("blocked sink unexpectedly drained")
	}
	if err := <-blocked; err != ErrStopped {
		t.Fatalf("blocked submit = %v", err)
	}
	close(gate)
}
