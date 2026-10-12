package tracing

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	tracingcore "github.com/xbcio/xbc/extensions/observability/tracing"
)

// The canonical HTTP spellings a configuration writes; the interceptor is
// expected to lower-case them once, because gRPC metadata keys are lower case.
const (
	defaultTraceIDHeader = "X-Trace-ID"
	defaultSpanIDHeader  = "X-Span-ID"
)

// recordingExporter keeps every exported span so a test can read what the
// interceptor recorded after a ForceFlush.
type recordingExporter struct {
	mu    sync.Mutex
	spans tracetest.SpanStubs
}

func (e *recordingExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, tracetest.SpanStubsFromReadOnlySpans(spans)...)
	return nil
}

func (*recordingExporter) Shutdown(context.Context) error { return nil }

func (e *recordingExporter) snapshot() tracetest.SpanStubs {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append(tracetest.SpanStubs(nil), e.spans...)
}

// testHandle stands in for the neutral plugin's private Handle: a real SDK
// provider over a recording exporter and the default W3C propagators, which is
// everything the interceptor consumes.
type testHandle struct {
	provider   *sdktrace.TracerProvider
	propagator propagation.TextMapPropagator
}

var _ tracingcore.Handle = (*testHandle)(nil)

func (h *testHandle) Provider() trace.TracerProvider { return h.provider }

func (h *testHandle) Tracer(name string, options ...trace.TracerOption) trace.Tracer {
	return h.provider.Tracer(name, options...)
}

func (h *testHandle) Extract(ctx context.Context, carrier propagation.TextMapCarrier) context.Context {
	return h.propagator.Extract(ctx, carrier)
}

func (h *testHandle) Inject(ctx context.Context, carrier propagation.TextMapCarrier) {
	h.propagator.Inject(ctx, carrier)
}

func (h *testHandle) ForceFlush(ctx context.Context) error { return h.provider.ForceFlush(ctx) }

func (h *testHandle) Shutdown(ctx context.Context) error { return h.provider.Shutdown(ctx) }

// newTestInterceptor builds the interceptor over a recording pipeline with the
// default correlation headers enabled, and shuts the pipeline down on cleanup.
func newTestInterceptor(t *testing.T) (*Interceptor, *testHandle, *recordingExporter) {
	t.Helper()
	exporter := new(recordingExporter)
	handle := &testHandle{
		provider: sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter)),
		propagator: propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{}, propagation.Baggage{},
		),
	}
	t.Cleanup(func() {
		if err := handle.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	headers := tracingcore.ResponseHeaders{Enabled: true, TraceIDHeader: defaultTraceIDHeader, SpanIDHeader: defaultSpanIDHeader}
	return newInterceptor(handle, headers), handle, exporter
}

// flush exports everything buffered and returns the spans.
func flush(t *testing.T, handle *testHandle, exporter *recordingExporter) tracetest.SpanStubs {
	t.Helper()
	if err := handle.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	return exporter.snapshot()
}

// spanAttribute reads one attribute out of an exported span.
func spanAttribute(attributes []attribute.KeyValue, key attribute.Key) (attribute.Value, bool) {
	for _, item := range attributes {
		if item.Key == key {
			return item.Value, true
		}
	}
	return attribute.Value{}, false
}
