package auditlog

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

const currentRouteKeyForTest = "xbc/web.currentRoute"

func initialized(t *testing.T, sink Sink, configure func(*Config)) *Plugin {
	t.Helper()
	cfg := DefaultConfig()
	if configure != nil {
		configure(&cfg)
	}
	p, err := New(cfg, WithSink(sink))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// auditEngine wires the middleware behind the two things it reads from the
// request: the frozen route and the principal. route.Path keeps the engine-
// neutral template the event is expected to carry; muxPattern translates it
// for registration, since the test engine spells wildcards ServeMux's way.
func auditEngine(p *Plugin, route web.RouteInfo, handler web.Handler) *enginetest.Engine {
	engine := enginetest.New()
	engine.Handle(route.Method, muxPattern(route.Path), []web.Handler{
		func(_ context.Context, c *web.Ctx) error {
			c.Set(currentRouteKeyForTest, route)
			c.Next()
			return nil
		},
		p.Handler(),
		func(_ context.Context, c *web.Ctx) error {
			web.SetPrincipal(c, web.Principal{Subject: "alice", AuthMethod: "apikey"})
			c.Next()
			return nil
		},
		handler,
	})
	return engine
}

// muxPattern rewrites a :name wildcard as ServeMux's {name}.
func muxPattern(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if strings.HasPrefix(segment, ":") {
			segments[i] = "{" + segment[1:] + "}"
		}
	}
	return strings.Join(segments, "/")
}

// noContent is the do-nothing route handler shared by the cases whose subject
// is the recorded event rather than the response body.
func noContent(_ context.Context, c *web.Ctx) error {
	c.Status(http.StatusNoContent)
	return nil
}

func TestRecordsRequiredMetadataWithoutBodiesOrKeyValue(t *testing.T) {
	sink := &memorySink{}
	p := initialized(t, sink, nil)
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders/:id", Name: "create-order"}
	engine := auditEngine(p, route, func(_ context.Context, c *web.Ctx) error {
		c.String(http.StatusCreated, "response-secret")
		return nil
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/orders/42", strings.NewReader("request-secret"))
	request.RemoteAddr = "192.0.2.10:1234"
	request.Header.Set("X-Request-ID", "req-123")
	request.Header.Set("Idempotency-Key", "never-log-this")
	engine.ServeHTTP(recorder, request)
	events, _ := sink.snapshot()
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	event := events[0]
	if event.Method != http.MethodPost || event.RouteTemplate != "/orders/:id" || event.RouteName != "create-order" || event.Status != http.StatusCreated || event.Bytes != len("response-secret") || event.ClientIP != "192.0.2.10" || event.Subject != "alice" || event.AuthMethod != "apikey" || event.RequestID != "req-123" || !event.IdempotencyKeyPresent || event.Panicked {
		t.Fatalf("event = %#v", event)
	}
	formatted := fmt.Sprintf("%#v", event)
	for _, secret := range []string{"request-secret", "response-secret", "never-log-this"} {
		if strings.Contains(formatted, secret) {
			t.Fatalf("event leaked %q: %s", secret, formatted)
		}
	}
}

func TestPanicIsRecordedAndRepanicked(t *testing.T) {
	sink := &memorySink{}
	p := initialized(t, sink, nil)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/panic"}
	engine := auditEngine(p, route, func(context.Context, *web.Ctx) error { panic("boom") })
	defer func() {
		recovered := recover()
		if recovered != "boom" {
			t.Fatalf("recovered = %#v", recovered)
		}
		events, _ := sink.snapshot()
		if len(events) != 1 || !events[0].Panicked || events[0].Status != 500 {
			t.Fatalf("events = %#v", events)
		}
	}()
	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panic", nil))
}

func TestSkipPathsAndCustomRequestIDExtractor(t *testing.T) {
	sink := &memorySink{}
	cfg := DefaultConfig()
	cfg.SkipPaths = []string{"/health*"}
	p, err := New(cfg, WithSink(sink), WithRequestIDExtractor(func(*web.Ctx) string { return "custom" }))
	if err != nil {
		t.Fatal(err)
	}
	health := web.RouteInfo{Method: http.MethodGet, Path: "/healthz"}
	auditEngine(p, health, noContent).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	events, _ := sink.snapshot()
	if len(events) != 0 {
		t.Fatalf("skipped events = %#v", events)
	}
	route := web.RouteInfo{Method: http.MethodGet, Path: "/ready"}
	auditEngine(p, route, noContent).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ready", nil))
	events, _ = sink.snapshot()
	if len(events) != 1 || events[0].RequestID != "custom" {
		t.Fatalf("events = %#v", events)
	}
}

func TestConcurrentRequestsAreRaceSafe(t *testing.T) {
	sink := &memorySink{}
	p := initialized(t, sink, nil)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/items/:id"}
	engine := auditEngine(p, route, noContent)
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/items/%d", i), nil)
			engine.ServeHTTP(httptest.NewRecorder(), request)
		}(i)
	}
	wg.Wait()
	events, _ := sink.snapshot()
	if len(events) != 100 {
		t.Fatalf("events = %d", len(events))
	}
}

type panicSink struct{}

func (panicSink) Write(context.Context, Event) error { panic("sink-boom") }

func TestSinkPanicDoesNotReplaceBusinessPanic(t *testing.T) {
	p := initialized(t, panicSink{}, nil)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/panic"}
	engine := auditEngine(p, route, func(context.Context, *web.Ctx) error { panic("business-boom") })
	defer func() {
		if recovered := recover(); recovered != "business-boom" {
			t.Fatalf("recovered = %#v", recovered)
		}
	}()
	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panic", nil))
}
