package auditlog

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication"
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
			web.SetPrincipal(c, authentication.Principal{Subject: "alice", AuthMethod: "apikey"})
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

// TestNoPrincipalRequestRecordsEmptySubject locks a deliberate decision, not a
// defect: observe reads the principal with `principal, _ :=
// web.CurrentPrincipal(c)` and never checks web.AuthenticationExempt, so a
// request with no published Principal (an exempt/public route, a rejected
// credential, or an authenticated-without-principal caller) records an empty
// Subject and AuthMethod rather than being skipped or refused. auditlog is an
// observability sidecar, not an authorization gate: it must keep recording
// every request Web actually serves, including every shape of unauthenticated
// one, or an operator loses visibility into exactly the traffic -- failed
// logins, scans against public routes -- audit logs exist to surface. This is
// why auditlog does not declare authentication.RequiresPrincipal: unlike
// tenant, idempotency, and casbin, it has no per-request check that a missing
// Principal should fail, so the marker would misstate its contract. Its Order
// (PhaseObserve, numerically before PhaseAuth) is also deliberately the
// opposite of a RequiresPrincipal pin: the hard phase boundary wraps
// authentication inside auditlog's own handler, so its deferred recording
// closure always observes the final outcome, authenticated or not, including
// a panic or an authentication rejection.
func TestNoPrincipalRequestRecordsEmptySubject(t *testing.T) {
	sink := &memorySink{}
	p := initialized(t, sink, nil)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/exempt"}
	engine := enginetest.New()
	engine.Handle(route.Method, route.Path, []web.Handler{
		func(_ context.Context, c *web.Ctx) error {
			c.Set(currentRouteKeyForTest, route)
			// Deliberately no web.SetPrincipal call: this is the no-principal
			// path (exempt route, or an authenticated-without-principal caller).
			c.Next()
			return nil
		},
		p.Handler(),
		noContent,
	})
	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/exempt", nil))
	events, _ := sink.snapshot()
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	if event := events[0]; event.Subject != "" || event.AuthMethod != "" {
		t.Fatalf("event = %#v, want empty Subject/AuthMethod (auditlog records every request; see observe's doc comment)", event)
	}
}

// TestPluginDoesNotDeclareRequiresPrincipal locks the decision in observe's
// doc comment: *Plugin must not implement authentication.RequiresPrincipal.
// auditlog has no per-request check that fails or skips recording when a
// Principal is missing -- it must keep observing every request Web actually
// serves -- so the marker would misstate its contract. If a future change
// makes *Plugin implement RequiresPrincipal, this test is the one to update
// deliberately alongside the doc comments on observe and the package.
func TestPluginDoesNotDeclareRequiresPrincipal(t *testing.T) {
	var value any = (*Plugin)(nil)
	if _, ok := value.(authentication.RequiresPrincipal); ok {
		t.Fatal("*Plugin must not implement authentication.RequiresPrincipal; see observe's doc comment")
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
