package tracing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/xbcio/xbc/transport/web"
)

const currentRouteKeyForTest = "xbc/web.currentRoute"

func init() { gin.SetMode(gin.TestMode) }

func TestMiddlewareExtractsW3CContextAndUsesFrozenRouteTemplate(t *testing.T) {
	const (
		incomingTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
		incomingSpanID  = "00f067aa0ba902b7"
	)
	exporter := new(recordingExporter)
	p := newTestPlugin(t, func(c *Config) { c.Batch.BatchTimeout = time.Hour }, exporter)

	var baggageValue string
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(currentRouteKeyForTest, web.RouteInfo{
			Method: http.MethodGet,
			Path:   "/users/:id",
			Name:   "users.get",
		})
		c.Next()
	})
	engine.Use(web.Handle(p.Handler()))
	engine.GET("/users/:id", func(c *gin.Context) {
		baggageValue = baggage.FromContext(c.Request.Context()).Member("tenant").Value()
		c.Status(http.StatusCreated)
	})

	request := httptest.NewRequest(http.MethodGet, "/users/42?secret=raw-url", nil)
	request.Header.Set("traceparent", "00-"+incomingTraceID+"-"+incomingSpanID+"-01")
	request.Header.Set("baggage", "tenant=acme")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d", response.Code)
	}
	if baggageValue != "acme" {
		t.Fatalf("extracted baggage = %q", baggageValue)
	}
	if err := p.Handle().ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}

	spans, _, _ := exporter.snapshot()
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != "GET /users/:id" || strings.Contains(span.Name, "/users/42") {
		t.Fatalf("span name = %q, want frozen route template", span.Name)
	}
	if span.SpanKind != trace.SpanKindServer {
		t.Fatalf("span kind = %v", span.SpanKind)
	}
	wantTraceID, err := trace.TraceIDFromHex(incomingTraceID)
	if err != nil {
		t.Fatal(err)
	}
	wantParentID, err := trace.SpanIDFromHex(incomingSpanID)
	if err != nil {
		t.Fatal(err)
	}
	if span.SpanContext.TraceID() != wantTraceID || span.Parent.SpanID() != wantParentID || !span.Parent.IsRemote() {
		t.Fatalf("span context = %v parent = %v", span.SpanContext, span.Parent)
	}
	if got := response.Header().Get(defaultTraceIDHeader); got != incomingTraceID {
		t.Fatalf("response trace ID = %q", got)
	}
	if got := response.Header().Get(defaultSpanIDHeader); got != span.SpanContext.SpanID().String() || got == incomingSpanID {
		t.Fatalf("response span ID = %q, span = %s parent = %s", got, span.SpanContext.SpanID(), incomingSpanID)
	}
	if got, ok := spanAttribute(span.Attributes, "http.route"); !ok || got.AsString() != "/users/:id" {
		t.Fatalf("http.route = %v, present=%v", got, ok)
	}
	if got, ok := spanAttribute(span.Attributes, "http.response.status_code"); !ok || got.AsInt64() != http.StatusCreated {
		t.Fatalf("http.response.status_code = %v, present=%v", got, ok)
	}
}

func TestMiddlewareRecordsAndRethrowsPanic(t *testing.T) {
	exporter := new(recordingExporter)
	p := newTestPlugin(t, func(c *Config) { c.Batch.BatchTimeout = time.Hour }, exporter)

	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(currentRouteKeyForTest, web.RouteInfo{Method: http.MethodGet, Path: "/panic"})
		c.Next()
	})
	engine.Use(web.Handle(p.Handler()))
	engine.GET("/panic", func(*gin.Context) { panic("boom") })

	func() {
		defer func() {
			if recovered := recover(); recovered != "boom" {
				t.Fatalf("recovered = %#v", recovered)
			}
		}()
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panic", nil))
	}()
	if err := p.Handle().ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans, _, _ := exporter.snapshot()
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Status.Code != codes.Error || len(span.Events) == 0 {
		t.Fatalf("panic span status/events = %#v/%#v", span.Status, span.Events)
	}
	if got, ok := spanAttribute(span.Attributes, "http.response.status_code"); !ok || got.AsInt64() != http.StatusInternalServerError {
		t.Fatalf("panic status attribute = %v, present=%v", got, ok)
	}
}

func TestMiddlewareUsesBoundedUnmatchedName(t *testing.T) {
	exporter := new(recordingExporter)
	p := newTestPlugin(t, func(c *Config) { c.Batch.BatchTimeout = time.Hour }, exporter)

	engine := gin.New()
	engine.Use(web.Handle(p.Handler()))
	engine.Handle("CUSTOM", "/raw/:id", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("CUSTOM", "/raw/secret", nil))
	if err := p.Handle().ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans, _, _ := exporter.snapshot()
	if len(spans) != 1 || spans[0].Name != "OTHER unmatched" || strings.Contains(spans[0].Name, "secret") {
		t.Fatalf("unmatched span = %#v", spans)
	}
}

func spanAttribute(attributes []attribute.KeyValue, key attribute.Key) (attribute.Value, bool) {
	for _, item := range attributes {
		if item.Key == key {
			return item.Value, true
		}
	}
	return attribute.Value{}, false
}
