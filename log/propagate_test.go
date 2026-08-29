package log

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TestInjectThenExtractInheritsTrace(t *testing.T) {
	upstream := NewTrace("client.call")
	h := http.Header{}
	Inject(WithTrace(context.Background(), upstream), propagation.HeaderCarrier(h))

	require.NotEmpty(t, h.Get("traceparent"), "Must write W3C standard header")
	assert.Equal(t, upstream.RequestID, h.Get(RequestIDHeader))

	ctx := Extract(context.Background(), propagation.HeaderCarrier(h), "GET /orders")
	got := TraceFrom(ctx)

	assert.Equal(t, upstream.TraceID(), got.TraceID(), "trace_id inheritance across processes")
	assert.Equal(t, upstream.SpanID(), got.ParentSpanID, "Upstream span becomes parent")
	assert.NotEqual(t, upstream.SpanID(), got.SpanID(), "New span opened in this process")
	assert.Equal(t, upstream.RequestID, got.RequestID)
	assert.Equal(t, "GET /orders", got.SpanName)
}

func TestExtractWithoutHeaderStartsNewTrace(t *testing.T) {
	ctx := Extract(context.Background(), propagation.HeaderCarrier(http.Header{}), "cron.job")

	tr := TraceFrom(ctx)
	assert.True(t, tr.Valid())
	assert.False(t, tr.ParentSpanID.IsValid())
	assert.Equal(t, "cron.job", tr.SpanName)
	assert.NotEmpty(t, tr.RequestID)
}

func TestExtractIgnoresMalformedTraceparent(t *testing.T) {
	h := http.Header{}
	h.Set("traceparent", "garbage")

	tr := TraceFrom(Extract(context.Background(), propagation.HeaderCarrier(h), "svc"))
	assert.True(t, tr.Valid(), "Downgrade to new trace when upstream header is malformed, cannot let request fail")
	assert.False(t, tr.ParentSpanID.IsValid())
}

// When upstream only supplies traceparent and not X-Request-Id, derive it
// from trace_id.
func TestExtractDerivesRequestIDFromTraceID(t *testing.T) {
	h := http.Header{}
	// The standard sample from the W3C spec document
	h.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")

	tr := TraceFrom(Extract(context.Background(), propagation.HeaderCarrier(h), "svc"))

	require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", tr.TraceID().String())
	assert.Equal(t, "00f067aa0ba902b7", tr.ParentSpanID.String())

	u, err := ulid.Parse(tr.RequestID)
	require.NoError(t, err)
	assert.Equal(t, tr.TraceID(), trace.TraceID(u), "request_id is ULID encoded trace_id")
}

func TestExtractPrefersUpstreamRequestID(t *testing.T) {
	h := http.Header{}
	h.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	h.Set(RequestIDHeader, "01M0RX90K2CGPJ7V41N2SN057D")

	tr := TraceFrom(Extract(context.Background(), propagation.HeaderCarrier(h), "svc"))
	assert.Equal(t, "01M0RX90K2CGPJ7V41N2SN057D", tr.RequestID, "Use upstream if provided")
}

func TestExtractBindsLoggerIntoContext(t *testing.T) {
	logs := installObserver(t)

	ctx := Extract(context.Background(), propagation.HeaderCarrier(http.Header{}), "GET /x")
	TInfo(ctx, "handled")

	require.Len(t, logs.All(), 1)
	m := logs.All()[0].ContextMap()
	assert.Equal(t, TraceFrom(ctx).TraceID().String(), m["trace_id"])
	assert.Equal(t, "GET /x", m["span_name"])
}

func TestInjectIsNoopWithoutTrace(t *testing.T) {
	h := http.Header{}
	Inject(context.Background(), propagation.HeaderCarrier(h))
	assert.Empty(t, h, "Do not write anything if no trace, do not create invalid traceparent")
}

// propagation.MapCarrier does not canonicalize keys the way http.Header
// does. A carrier that stores the request-id header in all lowercase (e.g.
// gRPC's metadata.MD convention) must still be recognized -- otherwise
// Extract silently falls back to deriving a different request_id from
// trace_id, breaking cross-service log correlation with no warning.
func TestExtractFindsLowercaseRequestIDOnNonCanonicalizingCarrier(t *testing.T) {
	upstreamRequestID := "01M0RX90K2CGPJ7V41N2SN057D"
	c := propagation.MapCarrier{
		"traceparent":                    "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		strings.ToLower(RequestIDHeader): upstreamRequestID,
	}

	tr := TraceFrom(Extract(context.Background(), c, "svc"))
	assert.Equal(t, upstreamRequestID, tr.RequestID,
		"x-request-id in lowercase must also match when carrier does not normalize case")
}

func TestExtractWithNilContext(t *testing.T) {
	//lint:ignore SA1012 explicitly verifying that a nil ctx does not panic
	var ctx context.Context //nolint:staticcheck
	assert.NotPanics(t, func() {
		ctx = Extract(ctx, propagation.HeaderCarrier(http.Header{}), "svc") //nolint:staticcheck
	})
	assert.True(t, TraceFrom(ctx).Valid())
}
