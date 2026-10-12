package httpclient

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// TestEachAttemptGetsItsOwnSpanAndTraceparent pins the per-attempt tracing
// shape: every attempt is a client span under the caller's span, and every
// attempt's request carries the traceparent of its own span -- so a backend
// sees three attempts where there were three, not one call counted twice.
func TestEachAttemptGetsItsOwnSpanAndTraceparent(t *testing.T) {
	handle := newFakeTracingHandle()
	t.Cleanup(func() { _ = handle.provider.Shutdown(context.Background()) })
	server := newScriptedServer(t, http.StatusServiceUnavailable, http.StatusOK)
	client := newTestClient(t, retryConfig(2), handle, nil)

	callerCtx, callerSpan := handle.provider.Tracer("caller").Start(context.Background(), "caller-operation")
	request := mustRequest(t, http.MethodGet, server.URL, nil).WithContext(callerCtx)
	response, err := client.Client().Do(request)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, 2, server.count())
	// Closing the final body ends its attempt's span, the way the caller of
	// the real client would end it by consuming the response.
	response.Body.Close()
	callerSpan.End()

	propagator := propagation.TraceContext{}
	firstSeen := trace.SpanContextFromContext(
		propagator.Extract(context.Background(), propagation.HeaderCarrier(server.header(0))))
	secondSeen := trace.SpanContextFromContext(
		propagator.Extract(context.Background(), propagation.HeaderCarrier(server.header(1))))
	require.True(t, firstSeen.IsValid(), "the first attempt carries a traceparent")
	require.True(t, secondSeen.IsValid(), "the retried attempt carries its own traceparent")
	assert.NotEqual(t, firstSeen.SpanID(), secondSeen.SpanID(),
		"each attempt injects the traceparent of its own span")
	assert.Equal(t, callerSpan.SpanContext().TraceID(), firstSeen.TraceID(),
		"both attempts stay in the caller's trace")

	var attempts []sdktrace.ReadOnlySpan
	for _, span := range handle.recorder.Ended() {
		if span.Name() == "HTTP GET" {
			attempts = append(attempts, span)
		}
	}
	require.Len(t, attempts, 2, "one span per attempt")
	for index, span := range attempts {
		assert.Equal(t, trace.SpanKindClient, span.SpanKind())
		assert.Equal(t, callerSpan.SpanContext().TraceID(), span.Parent().TraceID())
		assert.Equal(t, callerSpan.SpanContext().SpanID(), span.Parent().SpanID(),
			"the attempt's parent is the caller's span")
		wantSeen := firstSeen
		if index == 1 {
			wantSeen = secondSeen
		}
		assert.Equal(t, wantSeen.SpanID(), span.SpanContext().SpanID(),
			"the traceparent on the wire is this attempt's own span")
	}

	attributes := map[string]string{}
	for _, attribute := range attempts[0].Attributes() {
		attributes[string(attribute.Key)] = attribute.Value.AsString()
	}
	assert.Equal(t, "GET", attributes[string(semconv.HTTPRequestMethodKey)])
	assert.NotEmpty(t, attributes[string(semconv.ServerAddressKey)])
	assert.Equal(t, instrumentationName, attempts[0].InstrumentationScope().Name)
}

func TestNoTracingSelectionMeansNoTraceparent(t *testing.T) {
	server := newScriptedServer(t, http.StatusOK)
	client := newTestClient(t, Config{}, nil, nil)

	response, err := client.Client().Get(server.URL)
	require.NoError(t, err)
	defer response.Body.Close()
	assert.Empty(t, server.header(0).Get("traceparent"),
		"without the tracing plugin the request carries no propagation header")
}
