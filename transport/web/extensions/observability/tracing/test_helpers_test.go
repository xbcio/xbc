package tracing

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type recordingExporter struct {
	mu            sync.Mutex
	spans         tracetest.SpanStubs
	exportCalls   int
	shutdownCalls int
	exportErr     error
	shutdownErr   error
}

func (e *recordingExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.exportCalls++
	e.spans = append(e.spans, tracetest.SpanStubsFromReadOnlySpans(spans)...)
	return e.exportErr
}

func (e *recordingExporter) Shutdown(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.shutdownCalls++
	return e.shutdownErr
}

func (e *recordingExporter) snapshot() (tracetest.SpanStubs, int, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	spans := append(tracetest.SpanStubs(nil), e.spans...)
	return spans, e.exportCalls, e.shutdownCalls
}

type deadlineShutdownExporter struct {
	shutdownCalls atomic.Int32
}

func (*deadlineShutdownExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return nil
}

func (e *deadlineShutdownExporter) Shutdown(ctx context.Context) error {
	e.shutdownCalls.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

// newTestPlugin builds a Plugin from DefaultConfig (optionally mutated by
// configure) using exporter in place of the real OTLP factory, and registers
// a clean Stop as test cleanup. Tests that exercise Stop's own error paths or
// concurrency construct directly through newPlugin instead.
func newTestPlugin(t *testing.T, configure func(*Config), exporter sdktrace.SpanExporter) *Plugin {
	t.Helper()
	cfg := DefaultConfig()
	if configure != nil {
		configure(&cfg)
	}
	p, err := newPlugin(cfg, func(context.Context, normalizedConfig) (sdktrace.SpanExporter, error) {
		return exporter, nil
	})
	if err != nil {
		t.Fatalf("newPlugin() error = %v", err)
	}
	t.Cleanup(func() {
		if err := p.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return p
}
