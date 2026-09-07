package tracing

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/extensions/observability/accesslog"
)

func TestDefinitionIsCanonicalAndBundleIsStable(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero {
		t.Fatal("Definition() returned a zero handle")
	}
	if Definition() != Definition() {
		t.Fatal("Definition() returned different handles")
	}
	first, second := Bundle(), Bundle()
	if reflect.DeepEqual(first, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("Bundle() returned different composition content")
	}
}

func TestDefaultConfigActivatesUnderPluginsTracing(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestNewBuildsPipelineImmediatelyAndExposesMiddlewareContract(t *testing.T) {
	globalProvider := otel.GetTracerProvider()
	globalPropagator := otel.GetTextMapPropagator()

	exporter := new(recordingExporter)
	p := newTestPlugin(t, nil, exporter)

	if p.Order().Phase != web.PhaseObserve {
		t.Fatalf("Order().Phase = %v, want PhaseObserve", p.Order().Phase)
	}
	want := []web.OrderRef{web.Prefer(metricsKey), web.Prefer(accesslog.Key)}
	if !reflect.DeepEqual(p.Order().Before, want) {
		t.Fatalf("Order().Before = %#v, want %#v", p.Order().Before, want)
	}
	if p.Handler() == nil {
		t.Fatal("Handler() = nil")
	}
	if p.Handle() == nil || p.Handle().Provider() == nil {
		t.Fatal("Handle() did not publish a working provider")
	}
	if otel.GetTracerProvider() != globalProvider {
		t.Fatal("construction replaced the process-wide TracerProvider")
	}
	if otel.GetTextMapPropagator() != globalPropagator {
		t.Fatal("construction replaced the process-wide propagator")
	}
}

func TestBatchForceFlushExportsPendingSpan(t *testing.T) {
	exporter := new(recordingExporter)
	p := newTestPlugin(t, func(c *Config) { c.Batch.BatchTimeout = time.Hour }, exporter)

	_, span := p.Handle().Tracer("test").Start(context.Background(), "pending")
	span.End()
	if _, calls, _ := exporter.snapshot(); calls != 0 {
		t.Fatalf("export calls before flush = %d", calls)
	}
	if err := p.Handle().ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans, calls, _ := exporter.snapshot()
	if calls != 1 || len(spans) != 1 || spans[0].Name != "pending" {
		t.Fatalf("after flush: calls=%d spans=%#v", calls, spans)
	}
}

func TestSampleRatioZeroDropsRootSpans(t *testing.T) {
	exporter := new(recordingExporter)
	p := newTestPlugin(t, func(c *Config) { c.SampleRatio = 0 }, exporter)

	_, span := p.Handle().Tracer("test").Start(context.Background(), "drop-me")
	if span.IsRecording() || span.SpanContext().IsSampled() {
		t.Fatal("sample_ratio=0 recorded a root span")
	}
	span.End()
	if err := p.Handle().ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans, _, _ := exporter.snapshot()
	if len(spans) != 0 {
		t.Fatalf("exported %d spans with sample_ratio=0", len(spans))
	}
}

func TestHandleInjectUsesPrivateW3CPropagator(t *testing.T) {
	exporter := new(recordingExporter)
	p := newTestPlugin(t, nil, exporter)

	ctx, span := p.Handle().Tracer("test").Start(context.Background(), "inject")
	defer span.End()
	carrier := propagation.MapCarrier{}
	p.Handle().Inject(ctx, carrier)
	if carrier.Get("traceparent") == "" {
		t.Fatal("traceparent was not injected")
	}
}

func TestConcurrentStopShutsExporterDownOnce(t *testing.T) {
	exporter := new(recordingExporter)
	p, err := newPlugin(DefaultConfig(), func(context.Context, normalizedConfig) (sdktrace.SpanExporter, error) {
		return exporter, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	const callers = 32
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- p.Stop(context.Background())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}
	_, _, shutdowns := exporter.snapshot()
	if shutdowns != 1 {
		t.Fatalf("exporter shutdown calls = %d, want 1", shutdowns)
	}
}

func TestStopBoundsCleanupWithConfiguredExportTimeout(t *testing.T) {
	exporter := new(deadlineShutdownExporter)
	p, err := newPlugin(configuredWith(func(c *Config) { c.Batch.ExportTimeout = 20 * time.Millisecond }),
		func(context.Context, normalizedConfig) (sdktrace.SpanExporter, error) { return exporter, nil })
	if err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now()
	if err := p.Stop(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop error = %v, want configured cleanup deadline", err)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("bounded cleanup took %s", elapsed)
	}
	if calls := exporter.shutdownCalls.Load(); calls != 1 {
		t.Fatalf("exporter shutdown calls = %d, want 1", calls)
	}
}

func TestNewRollsBackExporterWhenFactoryFails(t *testing.T) {
	sentinel := errors.New("factory failed")
	exporter := new(recordingExporter)
	_, err := newPlugin(DefaultConfig(), func(context.Context, normalizedConfig) (sdktrace.SpanExporter, error) {
		return exporter, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("newPlugin() error = %v", err)
	}
	_, _, shutdowns := exporter.snapshot()
	if shutdowns != 1 {
		t.Fatalf("rollback shutdown calls = %d, want 1", shutdowns)
	}
}

func TestHandleShutdownCallerCancellationDoesNotCancelCleanup(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	shutdownErr := errors.New("shutdown failed")
	var flushCalls atomic.Int32
	var shutdownCalls atomic.Int32
	var cleanupCtx context.Context
	var cleanupCtxMu sync.Mutex
	h := newHandle(
		trace.NewNoopTracerProvider(),
		propagation.NewCompositeTextMapPropagator(),
		func(context.Context) error { flushCalls.Add(1); return nil },
		func(ctx context.Context) error {
			shutdownCalls.Add(1)
			cleanupCtxMu.Lock()
			cleanupCtx = ctx
			cleanupCtxMu.Unlock()
			close(started)
			select {
			case <-release:
				return shutdownErr
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		time.Second,
	)
	callerCtx, cancelCaller := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { first <- h.Shutdown(callerCtx) }()
	<-started
	cancelCaller()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first Shutdown error = %v, want caller cancellation", err)
	}
	cleanupCtxMu.Lock()
	cleanupErr := cleanupCtx.Err()
	cleanupCtxMu.Unlock()
	if cleanupErr != nil {
		t.Fatalf("internal cleanup context inherited caller cancellation: %v", cleanupErr)
	}
	later := make(chan error, 1)
	go func() { later <- h.Shutdown(context.Background()) }()
	select {
	case err := <-later:
		t.Fatalf("later Shutdown returned before cleanup completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-later; !errors.Is(err, shutdownErr) {
		t.Fatalf("later Shutdown error = %v, want shared shutdown error", err)
	}
	if err := h.Shutdown(context.Background()); !errors.Is(err, shutdownErr) {
		t.Fatalf("completed Shutdown error = %v, want shared shutdown error", err)
	}
	if flushCalls.Load() != 0 || shutdownCalls.Load() != 1 {
		t.Fatalf("force flush calls = %d, shutdown calls = %d", flushCalls.Load(), shutdownCalls.Load())
	}
}

func TestHandleShutdownUsesBoundedInternalContext(t *testing.T) {
	const cleanupTimeout = 20 * time.Millisecond
	var shutdownCalls atomic.Int32
	h := newHandle(
		trace.NewNoopTracerProvider(),
		propagation.NewCompositeTextMapPropagator(),
		func(context.Context) error { return nil },
		func(ctx context.Context) error {
			shutdownCalls.Add(1)
			<-ctx.Done()
			return ctx.Err()
		},
		cleanupTimeout,
	)
	startedAt := time.Now()
	if err := h.Shutdown(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want internal deadline", err)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("bounded cleanup took %s", elapsed)
	}
	if err := h.Shutdown(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("repeated Shutdown error = %v, want shared internal deadline", err)
	}
	if shutdownCalls.Load() != 1 {
		t.Fatalf("shutdown calls = %d, want 1", shutdownCalls.Load())
	}
}

// configuredWith returns DefaultConfig with mutate applied, for tests that
// build a Plugin directly through newPlugin rather than newTestPlugin.
func configuredWith(mutate func(*Config)) Config {
	cfg := DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	return cfg
}
