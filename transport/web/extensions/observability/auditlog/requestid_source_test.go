package auditlog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
	"github.com/xbcio/xbc/transport/web/extensions/observability/requestid"
)

// TestRequestIDComesFromTheRequestIDResponseHeader composes the real requestid
// middleware and checks that the audit event carries the value the framework
// published on the response -- not the inbound header, which requestid refused
// and replaced. The two chain orders pin the spec's claim that the deferred
// extraction works whether requestid is registered before or after auditlog.
func TestRequestIDComesFromTheRequestIDResponseHeader(t *testing.T) {
	const attacker = "attacker-chosen!"
	for _, tt := range []struct {
		name    string
		request int
	}{
		{name: "requestid before auditlog", request: 0},
		{name: "requestid after auditlog", request: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sink := &memorySink{}
			p := initialized(t, sink, nil)
			route := web.RouteInfo{Method: http.MethodGet, Path: "/orders"}
			chain := []web.Handler{routePublisher(route)}
			if tt.request == 0 {
				chain = append(chain, requestid.New().Handler(), p.Handler())
			} else {
				chain = append(chain, p.Handler(), requestid.New().Handler())
			}
			chain = append(chain, noContent)

			engine := enginetest.New()
			engine.Handle(route.Method, route.Path, chain)
			request := httptest.NewRequest(http.MethodGet, "/orders", nil)
			request.Header.Set("X-Request-ID", attacker)
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)

			events, _ := sink.snapshot()
			if len(events) != 1 {
				t.Fatalf("events = %#v", events)
			}
			want := response.Header().Get("X-Request-ID")
			if want == "" || want == attacker {
				t.Fatalf("response request ID = %q, want the ID requestid generated", want)
			}
			if events[0].RequestID != want {
				t.Fatalf("event request ID = %q, want the response header %q", events[0].RequestID, want)
			}
		})
	}
}

// TestInboundHeaderIsNotPromotedWithoutRequestID pins the consequence of
// reading the response header: with no requestid middleware there is no
// framework request ID, so an inbound header stays out of the audit record
// instead of being promoted to one.
func TestInboundHeaderIsNotPromotedWithoutRequestID(t *testing.T) {
	sink := &memorySink{}
	p := initialized(t, sink, nil)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/orders"}
	engine := auditEngine(p, route, noContent)
	request := httptest.NewRequest(http.MethodGet, "/orders", nil)
	request.Header.Set("X-Request-ID", "attacker-supplied")
	engine.ServeHTTP(httptest.NewRecorder(), request)

	events, _ := sink.snapshot()
	if len(events) != 1 {
		t.Fatalf("events = %#v", events)
	}
	if events[0].RequestID != "" {
		t.Fatalf("event request ID = %q, want empty without requestid", events[0].RequestID)
	}
}

// TestResponseRequestIDAcceptsTheFrameworkMaximum pins the upper side of the
// extractor's bound: requestid accepts a configured max_length up to 1024 and
// the access log records values up to 1024 bytes, so an ID at that limit must
// reach the audit event rather than being dropped by a narrower check.
func TestResponseRequestIDAcceptsTheFrameworkMaximum(t *testing.T) {
	const size = 1024
	value := strings.Repeat("a", size)
	sink := &memorySink{}
	p := initialized(t, sink, nil)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/orders"}
	engine := auditEngine(p, route, func(_ context.Context, c *web.Ctx) error {
		c.SetHeader("X-Request-ID", value)
		c.Status(http.StatusNoContent)
		return nil
	})
	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/orders", nil))

	events, _ := sink.snapshot()
	if len(events) != 1 {
		t.Fatalf("events = %#v", events)
	}
	if events[0].RequestID != value {
		t.Fatalf("event request ID length = %d, want the %d-byte response header carried", len(events[0].RequestID), size)
	}
}

// TestInvalidResponseRequestIDYieldsEmptyField keeps the length and
// control-character checks as defence in depth over whatever a middleware
// writes onto the response, matching the previous rejection of an invalid
// request header: the field is empty, never truncated.
func TestInvalidResponseRequestIDYieldsEmptyField(t *testing.T) {
	for name, value := range map[string]string{
		"oversized":         strings.Repeat("a", 1025),
		"control character": "req\ninjected",
	} {
		t.Run(name, func(t *testing.T) {
			sink := &memorySink{}
			p := initialized(t, sink, nil)
			route := web.RouteInfo{Method: http.MethodGet, Path: "/orders"}
			engine := auditEngine(p, route, func(_ context.Context, c *web.Ctx) error {
				c.SetHeader("X-Request-ID", value)
				c.Status(http.StatusNoContent)
				return nil
			})
			engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/orders", nil))

			events, _ := sink.snapshot()
			if len(events) != 1 {
				t.Fatalf("events = %#v", events)
			}
			if events[0].RequestID != "" {
				t.Fatalf("event request ID = %q, want empty for an invalid response header", events[0].RequestID)
			}
		})
	}
}

// routePublisher publishes the frozen route the audit event reports, the same
// seam auditEngine uses for its own chains.
func routePublisher(route web.RouteInfo) web.Handler {
	return func(_ context.Context, c *web.Ctx) error {
		c.Set(currentRouteKeyForTest, route)
		c.Next()
		return nil
	}
}
