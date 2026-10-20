package idempotency

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

const currentRouteKeyForTest = "xbc/web.currentRoute"

func initializedPlugin(t *testing.T, configure func(*Config), opts ...Option) *Plugin {
	t.Helper()
	cfg := DefaultConfig()
	if configure != nil {
		configure(&cfg)
	}
	p, err := New(cfg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func engineFor(p *Plugin, route web.RouteInfo, handler web.Handler) *enginetest.Engine {
	engine := enginetest.New()
	engine.Handle(route.Method, route.Path, []web.Handler{
		func(_ context.Context, c *web.Ctx) error {
			c.Set(currentRouteKeyForTest, route)
			web.SetPrincipal(c, authentication.Principal{Subject: "alice", AuthMethod: "test"})
			c.Next()
			return nil
		},
		p.Handler(),
		handler,
	})
	return engine
}

// noContent is the do-nothing route handler shared by the cases whose subject
// is the middleware rather than the response body.
func noContent(_ context.Context, c *web.Ctx) error {
	c.Status(http.StatusNoContent)
	return nil
}

func perform(engine http.Handler, key, body string) *httptest.ResponseRecorder {
	return performPath(engine, "/orders", key, body)
}

func performPath(engine http.Handler, requestPath, key, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, requestPath, strings.NewReader(body))
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	engine.ServeHTTP(recorder, request)
	return recorder
}

func TestCompletedResponseIsSafelyReplayed(t *testing.T) {
	p := initializedPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	var calls atomic.Int32
	engine := engineFor(p, route, func(_ context.Context, c *web.Ctx) error {
		calls.Add(1)
		data, _ := io.ReadAll(c.Request().Body)
		c.Data(http.StatusCreated, "application/json", append([]byte(`{"body":`), append(data, '}')...))
		return nil
	})
	first := perform(engine, "order-123456", `"one"`)
	second := perform(engine, "order-123456", `"one"`)
	// Result().Header is the header snapshot taken when the response was
	// committed. recorder.Header() is the live map, which reads back a header
	// set after the status line just as happily -- and such a header never
	// reaches a real client, so the replay marker would be unobservable
	// exactly where it matters.
	replayed := second.Result().Header.Get("Idempotency-Replayed")
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated || first.Body.String() != second.Body.String() || replayed != "true" || calls.Load() != 1 {
		t.Fatalf("first=%d %q second=%d %q replay=%q calls=%d", first.Code, first.Body.String(), second.Code, second.Body.String(), replayed, calls.Load())
	}
	conflict := perform(engine, "order-123456", `"two"`)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d", conflict.Code)
	}
}

// TestConcreteRequestPathsAreIndependentIdempotencyScopes locks the scope of a
// stored response to the path the engine actually routed, not to the template
// that matched it. One subject reusing one Idempotency-Key for two different
// order ids must execute both cancellations; only a repeat of the exact same
// concrete path may replay.
func TestConcreteRequestPathsAreIndependentIdempotencyScopes(t *testing.T) {
	p := initializedPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders/{id}/cancel", Idempotent: true}
	var calls atomic.Int32
	engine := enginetest.New()
	engine.Handle(route.Method, route.Path, []web.Handler{
		func(_ context.Context, c *web.Ctx) error {
			c.Set(currentRouteKeyForTest, route)
			web.SetPrincipal(c, authentication.Principal{Subject: "alice", AuthMethod: "test"})
			c.Next()
			return nil
		},
		p.Handler(),
		func(_ context.Context, c *web.Ctx) error {
			calls.Add(1)
			c.String(http.StatusOK, "call-%d", calls.Load())
			return nil
		},
	})
	first := performPath(engine, "/orders/1/cancel", "cancel-key", `{}`)
	second := performPath(engine, "/orders/2/cancel", "cancel-key", `{}`)
	if first.Body.String() != "call-1" || second.Body.String() != "call-2" {
		t.Fatalf("first=%q second=%q, want each concrete path to execute", first.Body.String(), second.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
	if got := second.Result().Header.Get("Idempotency-Replayed"); got != "" {
		t.Fatalf("second concrete path replayed the first response: %q", got)
	}
	repeat := performPath(engine, "/orders/1/cancel", "cancel-key", `{}`)
	if repeat.Body.String() != "call-1" || repeat.Result().Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("repeat body=%q replay=%q, want the first concrete path replayed", repeat.Body.String(), repeat.Result().Header.Get("Idempotency-Replayed"))
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d after replay, want 2", calls.Load())
	}
}

// TestReplayRestoresWhitelistedHeadersAndNeverSetCookie locks the replay
// contract for headers: a retry must observe the same Location, validators and
// caching directives the original response carried, while Set-Cookie is never
// reattached to a later request.
func TestReplayRestoresWhitelistedHeadersAndNeverSetCookie(t *testing.T) {
	p := initializedPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	var calls atomic.Int32
	engine := engineFor(p, route, func(_ context.Context, c *web.Ctx) error {
		calls.Add(1)
		c.SetHeader("Location", "/orders/42")
		c.SetHeader("Content-Language", "en")
		c.SetHeader("Content-Disposition", `attachment; filename="order.json"`)
		c.SetHeader("ETag", `"v42"`)
		c.SetHeader("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
		c.SetHeader("Cache-Control", "no-store")
		c.SetHeader("Set-Cookie", "session=original")
		c.Data(http.StatusCreated, "application/json", []byte(`{"id":42}`))
		return nil
	})
	first := perform(engine, "header-replay-key", `{}`)
	second := perform(engine, "header-replay-key", `{}`)
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated || calls.Load() != 1 {
		t.Fatalf("first=%d second=%d calls=%d", first.Code, second.Code, calls.Load())
	}
	if got := first.Result().Header.Get("Set-Cookie"); got != "session=original" {
		t.Fatalf("original response lost its Set-Cookie: %q", got)
	}
	want := map[string]string{
		"Content-Type":        "application/json",
		"Location":            "/orders/42",
		"Content-Language":    "en",
		"Content-Disposition": `attachment; filename="order.json"`,
		"ETag":                `"v42"`,
		"Last-Modified":       "Wed, 21 Oct 2015 07:28:00 GMT",
		"Cache-Control":       "no-store",
	}
	replayed := second.Result().Header
	for name, value := range want {
		if got := replayed.Get(name); got != value {
			t.Fatalf("replayed %s = %q, want %q", name, got, value)
		}
	}
	if got := replayed.Values("Set-Cookie"); len(got) != 0 {
		t.Fatalf("replayed Set-Cookie = %q, want none", got)
	}
	if got := replayed.Get("Idempotency-Replayed"); got != "true" {
		t.Fatalf("Idempotency-Replayed = %q", got)
	}
}

// TestReplayRefusesStoreResponsesWithUnsafeHeaders locks the replay validation
// boundary: a Store implementation (or a corrupted record) cannot inject
// Set-Cookie or an arbitrary header into a replayed response.
func TestReplayRefusesStoreResponsesWithUnsafeHeaders(t *testing.T) {
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	cases := map[string]Response{
		"set-cookie": responseWithHeader("Set-Cookie", "session=stolen"),
		"unknown":    responseWithHeader("X-Injected", "value"),
		"control":    responseWithHeader("Location", "/ok\r\nX-Injected: value"),
	}
	for name, response := range cases {
		p := initializedPlugin(t, nil, WithStore(completedStore{response: response}))
		if got := perform(engineFor(p, route, noContent), "unsafe-header-"+name, "").Code; got != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503", name, got)
		}
	}
}

func responseWithHeader(name, value string) Response {
	header := http.Header{}
	header.Set(name, value)
	return Response{Status: http.StatusOK, Header: header}
}

// TestReplayDropsCapturedHeaderValuesWithControlCharacters proves capture
// sanitizes at the source: a handler cannot poison its own record, so the
// record is still replayable and simply omits the unusable value.
func TestReplayDropsCapturedHeaderValuesWithControlCharacters(t *testing.T) {
	p := initializedPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	engine := engineFor(p, route, func(_ context.Context, c *web.Ctx) error {
		c.SetHeader("Location", "/ok\r\nX-Injected: value")
		c.SetHeader("Cache-Control", "no-store")
		c.String(http.StatusOK, "done")
		return nil
	})
	if got := perform(engine, "control-header-key", "").Code; got != http.StatusOK {
		t.Fatalf("first = %d", got)
	}
	replayed := perform(engine, "control-header-key", "").Result()
	if replayed.StatusCode != http.StatusOK {
		t.Fatalf("replay = %d, want a normal replay", replayed.StatusCode)
	}
	if got := replayed.Header.Get("Location"); got != "" {
		t.Fatalf("replayed Location = %q, want the unsafe value dropped at capture", got)
	}
	if got := replayed.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("replayed Cache-Control = %q, want no-store", got)
	}
}

// TestCaptureAppliesReplayHeaderBounds locks that capture applies the same
// bounds replay validation enforces. A whitelisted header the handler sets
// beyond those bounds must be dropped when the record is written, not stored
// and then rejected on every retry: the record has to stay replayable.
func TestCaptureAppliesReplayHeaderBounds(t *testing.T) {
	p := initializedPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	var calls atomic.Int32
	oversized := "/" + strings.Repeat("a", maxReplayHeaderValueBytes)
	engine := engineFor(p, route, func(_ context.Context, c *web.Ctx) error {
		calls.Add(1)
		c.SetHeader("Location", oversized)
		c.SetHeader("ETag", `"kept"`)
		c.String(http.StatusOK, "done")
		return nil
	})
	if got := perform(engine, "oversized-header-key", "").Code; got != http.StatusOK {
		t.Fatalf("first = %d", got)
	}
	replay := perform(engine, "oversized-header-key", "")
	if replay.Code != http.StatusOK {
		t.Fatalf("replay = %d, want the oversized value dropped at capture", replay.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want the record replayed", calls.Load())
	}
	if got := replay.Result().Header.Get("Location"); got != "" {
		t.Fatalf("replayed Location = %d bytes, want it dropped at capture", len(got))
	}
	if got := replay.Result().Header.Get("ETag"); got != `"kept"` {
		t.Fatalf("replayed ETag = %q, want the in-bounds value kept", got)
	}
}

// TestCaptureAppliesReplayHeaderTotalBound covers the aggregate bound: values
// that individually fit but together exceed the total budget are dropped at
// capture so the stored record remains replayable.
func TestCaptureAppliesReplayHeaderTotalBound(t *testing.T) {
	p := initializedPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	value := strings.Repeat("b", maxReplayHeaderValueBytes)
	var calls atomic.Int32
	engine := engineFor(p, route, func(_ context.Context, c *web.Ctx) error {
		calls.Add(1)
		for range 9 {
			c.Writer().Header().Add("Cache-Control", value)
		}
		c.String(http.StatusOK, "done")
		return nil
	})
	if got := perform(engine, "oversized-total-key", "").Code; got != http.StatusOK {
		t.Fatalf("first = %d", got)
	}
	replay := perform(engine, "oversized-total-key", "")
	if replay.Code != http.StatusOK {
		t.Fatalf("replay = %d, want the over-budget values dropped at capture", replay.Code)
	}
	values := replay.Result().Header.Values("Cache-Control")
	if len(values) == 0 || len(values) >= 9 {
		t.Fatalf("replayed Cache-Control values = %d, want some but not all", len(values))
	}
	total := 0
	for _, value := range values {
		total += len("Cache-Control") + len(value)
	}
	if total > maxReplayHeaderTotalBytes {
		t.Fatalf("replayed header total = %d, want <= %d", total, maxReplayHeaderTotalBytes)
	}
}

// TestCaptureDropsOversizedContentType covers the Content-Type field, which is
// persisted separately but bounded by the same replay validation.
func TestCaptureDropsOversizedContentType(t *testing.T) {
	p := initializedPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	var calls atomic.Int32
	engine := engineFor(p, route, func(_ context.Context, c *web.Ctx) error {
		calls.Add(1)
		c.SetHeader("Content-Type", strings.Repeat("c", maxReplayHeaderValueBytes+1))
		c.Status(http.StatusOK)
		return nil
	})
	if got := perform(engine, "oversized-content-type", "").Code; got != http.StatusOK {
		t.Fatalf("first = %d", got)
	}
	replay := perform(engine, "oversized-content-type", "")
	if replay.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("replay = %d calls = %d, want a successful replay", replay.Code, calls.Load())
	}
	if got := replay.Result().Header.Get("Content-Type"); got != "" {
		t.Fatalf("replayed Content-Type = %d bytes, want it dropped at capture", len(got))
	}
}

// TestReplayRefusesHeaderValuesBeyondReplayBounds covers the two bounds replay
// validation enforces on a Store-supplied response: one oversized value, and
// many individually valid values whose total exceeds the budget.
func TestReplayRefusesHeaderValuesBeyondReplayBounds(t *testing.T) {
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	single := responseWithHeader("Location", "/"+strings.Repeat("a", maxReplayHeaderValueBytes))
	total := Response{Status: http.StatusOK, Header: http.Header{}}
	for range 9 {
		total.Header.Add("Cache-Control", strings.Repeat("b", maxReplayHeaderValueBytes))
	}
	cases := map[string]Response{"single-value": single, "total": total}
	for name, response := range cases {
		p := initializedPlugin(t, nil, WithStore(completedStore{response: response}))
		if got := perform(engineFor(p, route, noContent), "replay-bounds-"+name, "").Code; got != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503", name, got)
		}
	}
}

// TestReplayRefusesOversizedStoreContentType locks the replay-side bound on the
// Content-Type field: a Store (or a corrupted record) supplying a value beyond
// the capture bound must produce 503 idempotency_unavailable, not a replay.
func TestReplayRefusesOversizedStoreContentType(t *testing.T) {
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	response := Response{Status: http.StatusOK, ContentType: strings.Repeat("c", 2*maxReplayHeaderValueBytes)}
	p := initializedPlugin(t, nil, WithStore(completedStore{response: response}))
	if got := perform(engineFor(p, route, noContent), "oversized-store-content-type", "").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for an oversized stored Content-Type", got)
	}
}

// TestReplayReplacesPreexistingHeadersInsteadOfAppending locks the Del before
// Add: middleware ahead of idempotency may already have set one of the
// whitelisted names on the outgoing response, and a replay must reproduce the
// stored value rather than append a second one.
func TestReplayReplacesPreexistingHeadersInsteadOfAppending(t *testing.T) {
	p := initializedPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	engine := enginetest.New()
	engine.Handle(route.Method, route.Path, []web.Handler{
		func(_ context.Context, c *web.Ctx) error {
			c.Set(currentRouteKeyForTest, route)
			web.SetPrincipal(c, authentication.Principal{Subject: "alice", AuthMethod: "test"})
			c.Next()
			return nil
		},
		func(_ context.Context, c *web.Ctx) error {
			c.SetHeader("Cache-Control", "upstream")
			c.SetHeader("Location", "/upstream")
			c.Next()
			return nil
		},
		p.Handler(),
		func(_ context.Context, c *web.Ctx) error {
			c.SetHeader("Cache-Control", "no-store")
			c.SetHeader("Location", "/stored")
			c.String(http.StatusOK, "stored")
			return nil
		},
	})
	perform(engine, "replace-header-key", "")
	replay := perform(engine, "replace-header-key", "").Result()
	values := replay.Header.Values("Cache-Control")
	if len(values) != 1 || values[0] != "no-store" {
		t.Fatalf("replayed Cache-Control = %q, want only the stored value", values)
	}
	if got := replay.Header.Get("Location"); got != "/stored" {
		t.Fatalf("replayed Location = %q, want the stored value", got)
	}
}

// TestCaptureDropsEmptyHeaderValues locks that an explicitly empty whitelisted
// value is absent from the record, so a replay cannot resurrect it as a
// present-but-empty header.
func TestCaptureDropsEmptyHeaderValues(t *testing.T) {
	p := initializedPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	engine := engineFor(p, route, func(_ context.Context, c *web.Ctx) error {
		c.Writer().Header().Set("Location", "")
		c.SetHeader("Cache-Control", "no-store")
		c.String(http.StatusOK, "done")
		return nil
	})
	perform(engine, "empty-header-key", "")
	replay := perform(engine, "empty-header-key", "").Result()
	if got := replay.Header.Values("Location"); len(got) != 0 {
		t.Fatalf("replayed Location = %q, want the empty value dropped at capture", got)
	}
	if got := replay.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("replayed Cache-Control = %q", got)
	}
}

func TestConcurrentDuplicateRunsHandlerOnce(t *testing.T) {
	p := initializedPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	engine := engineFor(p, route, func(_ context.Context, c *web.Ctx) error {
		calls.Add(1)
		close(entered)
		<-release
		c.String(http.StatusOK, "done")
		return nil
	})
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- perform(engine, "concurrent-key", `{}`) }()
	<-entered
	second := perform(engine, "concurrent-key", `{}`)
	if second.Code != http.StatusTooEarly {
		t.Fatalf("pending status = %d body=%s", second.Code, second.Body.String())
	}
	close(release)
	first := <-firstDone
	if first.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("first=%d calls=%d", first.Code, calls.Load())
	}
}

func TestOnlyMarkedRoutesApplyAndFailuresRelease(t *testing.T) {
	p := initializedPlugin(t, func(c *Config) { c.MaxResponseBytes = 4 })
	unmarked := web.RouteInfo{Method: http.MethodPost, Path: "/orders"}
	if got := perform(engineFor(p, unmarked, noContent), "", "").Code; got != http.StatusNoContent {
		t.Fatalf("unmarked = %d", got)
	}
	marked := unmarked
	marked.Idempotent = true
	var calls atomic.Int32
	engine := engineFor(p, marked, func(_ context.Context, c *web.Ctx) error {
		if calls.Add(1) == 1 {
			c.String(http.StatusInternalServerError, "failed")
		} else {
			c.String(http.StatusOK, "response-too-large")
		}
		return nil
	})
	if got := perform(engine, "retry-key", "").Code; got != http.StatusInternalServerError {
		t.Fatalf("failed = %d", got)
	}
	if got := perform(engine, "retry-key", "").Code; got != http.StatusOK {
		t.Fatalf("retry = %d", got)
	}
	if got := perform(engine, "retry-key", "").Code; got != http.StatusOK || calls.Load() != 3 {
		t.Fatalf("oversize was cached: status=%d calls=%d", got, calls.Load())
	}
}

func TestKeyAndRequestBodyLimits(t *testing.T) {
	p := initializedPlugin(t, func(c *Config) { c.MaxRequestBytes = 2 })
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	engine := engineFor(p, route, noContent)
	if got := perform(engine, "", "").Code; got != http.StatusBadRequest {
		t.Fatalf("missing key = %d", got)
	}
	if got := perform(engine, "valid-key", "abc").Code; got != http.StatusRequestEntityTooLarge {
		t.Fatalf("large body = %d", got)
	}
}

func TestQueryAndPrincipalScopeArePartOfSemantics(t *testing.T) {
	p := initializedPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	var calls atomic.Int32
	engine := engineFor(p, route, func(_ context.Context, c *web.Ctx) error {
		calls.Add(1)
		c.Status(http.StatusNoContent)
		return nil
	})
	if got := performPath(engine, "/orders?mode=fast", "semantic-key", `{}`).Code; got != http.StatusNoContent {
		t.Fatalf("first = %d", got)
	}
	if got := performPath(engine, "/orders?mode=slow", "semantic-key", `{}`).Code; got != http.StatusConflict {
		t.Fatalf("query conflict = %d", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

// TestPrincipalScopeSeparatesIdenticalRequests locks the cross-caller isolation
// guarantee: two principals presenting the same idempotency key, method,
// concrete path, query, content type and body occupy different scopes. Neither
// may replay the other's response; both handlers execute and each caller
// receives its own result.
func TestPrincipalScopeSeparatesIdenticalRequests(t *testing.T) {
	p := initializedPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	var calls atomic.Int32
	engine := enginetest.New()
	engine.Handle(route.Method, route.Path, []web.Handler{
		func(_ context.Context, c *web.Ctx) error {
			c.Set(currentRouteKeyForTest, route)
			web.SetPrincipal(c, authentication.Principal{Subject: c.GetHeader("X-Test-Principal"), AuthMethod: "test"})
			c.Next()
			return nil
		},
		p.Handler(),
		func(_ context.Context, c *web.Ctx) error {
			calls.Add(1)
			principal, _ := web.CurrentPrincipal(c)
			c.String(http.StatusOK, "principal:%s", principal.Subject)
			return nil
		},
	})
	alice := performAsPrincipal(engine, "shared-key", `{}`, "alice")
	bob := performAsPrincipal(engine, "shared-key", `{}`, "bob")
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want both principals to execute", calls.Load())
	}
	for name, response := range map[string]*httptest.ResponseRecorder{"alice": alice, "bob": bob} {
		if got := response.Result().Header.Get("Idempotency-Replayed"); got != "" {
			t.Fatalf("%s replayed = %q, want no replay between principals", name, got)
		}
	}
	if got := alice.Body.String(); got != "principal:alice" {
		t.Fatalf("alice body = %q", got)
	}
	if got := bob.Body.String(); got != "principal:bob" {
		t.Fatalf("bob body = %q, want his own response rather than alice's", got)
	}
}

func performAsPrincipal(engine http.Handler, key, body, subject string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(body))
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set("X-Test-Principal", subject)
	engine.ServeHTTP(recorder, request)
	return recorder
}

// TestRequestContentTypeIsPartOfTheFingerprint locks Content-Type as a
// documented fingerprint input: the same principal, key, concrete path and body
// under a different request Content-Type is a different request and must not
// replay the stored response.
func TestRequestContentTypeIsPartOfTheFingerprint(t *testing.T) {
	p := initializedPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	var calls atomic.Int32
	engine := engineFor(p, route, func(_ context.Context, c *web.Ctx) error {
		calls.Add(1)
		c.Status(http.StatusNoContent)
		return nil
	})
	first := performWithContentType(engine, "application/json", "content-type-key", `{}`)
	second := performWithContentType(engine, "text/plain", "content-type-key", `{}`)
	if first.Code != http.StatusNoContent {
		t.Fatalf("first = %d, want 204", first.Code)
	}
	if second.Code != http.StatusConflict {
		t.Fatalf("second = %d, want 409 idempotency_conflict", second.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want only the first request to execute", calls.Load())
	}
}

func performWithContentType(engine http.Handler, contentType, key, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(body))
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set("Content-Type", contentType)
	engine.ServeHTTP(recorder, request)
	return recorder
}

// TestNoPrincipalRequestWithIdempotencyKeyIsRefused locks the fixed semantics:
// idempotency requires a caller identity to scope a stored response to. A
// request that carries the Idempotency-Key header but reaches this middleware
// with no published Principal (an exempt/public route, or an
// authenticated-without-principal caller) is refused before the store is ever
// touched -- it must not share a storageKey with, or be able to replay, any
// other caller's response. This replaces the pre-existing "locks the defect"
// test: see the authentication-model-gaps plan's Out of Scope table and this
// batch's roadmap entry for the prior collapse this closes.
func TestNoPrincipalRequestWithIdempotencyKeyIsRefused(t *testing.T) {
	p := initializedPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	var calls atomic.Int32
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
		func(_ context.Context, c *web.Ctx) error {
			calls.Add(1)
			c.Status(http.StatusNoContent)
			return nil
		},
	})
	// Two unrelated subjectless callers presenting the very same key must both
	// be refused outright, not folded into one storage slot or allowed to
	// replay each other's response.
	first := perform(engine, "shared-key", `{}`)
	second := perform(engine, "shared-key", `{}`)
	if first.Code != http.StatusForbidden || second.Code != http.StatusForbidden {
		t.Fatalf("first=%d second=%d, want both refused", first.Code, second.Code)
	}
	if replayed := second.Result().Header.Get("Idempotency-Replayed"); replayed != "" || calls.Load() != 0 {
		t.Fatalf("replayed=%q calls=%d, want no replay and no handler execution", replayed, calls.Load())
	}
}

// TestNoPrincipalRequestWithoutIdempotencyKeyIsUnaffected locks that the new
// refusal is scoped to requests that actually carry the header: a route that
// is not marked Route.Idempotent, or one where the caller sent no
// Idempotency-Key, behaves exactly as it did before a Principal existed at
// all -- idempotency never requires authentication by itself, only when a
// client opts into the idempotent-request contract.
func TestNoPrincipalRequestWithoutIdempotencyKeyIsUnaffected(t *testing.T) {
	p := initializedPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	engine := enginetest.New()
	engine.Handle(route.Method, route.Path, []web.Handler{
		func(_ context.Context, c *web.Ctx) error {
			c.Set(currentRouteKeyForTest, route)
			c.Next()
			return nil
		},
		p.Handler(),
		noContent,
	})
	if got := perform(engine, "", "").Code; got != http.StatusBadRequest {
		t.Fatalf("no-key request = %d, want the pre-existing missing-key rejection", got)
	}
}

func TestDuplicateKeyHeaderAndInvalidInjectedReplayAreRejected(t *testing.T) {
	p := initializedPlugin(t, nil)
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	engine := engineFor(p, route, noContent)
	req := httptest.NewRequest(http.MethodPost, "/orders", nil)
	req.Header.Add("Idempotency-Key", "duplicate-one")
	req.Header.Add("Idempotency-Key", "duplicate-two")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("duplicate status = %d", recorder.Code)
	}

	bad := initializedPlugin(t, func(c *Config) { c.MaxResponseBytes = 4 },
		WithStore(completedStore{response: Response{Status: 200, Body: make([]byte, 32)}}))
	response := perform(engineFor(bad, route, noContent), "invalid-store", "")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("invalid replay status = %d", response.Code)
	}
}

type completedStore struct{ response Response }

func (s completedStore) Acquire(context.Context, string, string, string, time.Duration) (AcquireResult, error) {
	return AcquireResult{State: Completed, Response: s.response}, nil
}
func (completedStore) Complete(context.Context, string, string, string, Response, time.Duration) error {
	return nil
}
func (completedStore) Release(context.Context, string, string, string) error { return nil }

// TestStoreAcquireErrorsMapToUnavailable locks both store-error shapes the
// middleware must absorb: the memory backend's admission refusal and a generic
// transport failure. Both are reported as 503 idempotency_unavailable, and
// neither may leave cleanup behind: Acquire failed, so the middleware never
// owned a reservation to complete or release.
func TestStoreAcquireErrorsMapToUnavailable(t *testing.T) {
	route := web.RouteInfo{Method: http.MethodPost, Path: "/orders", Idempotent: true}
	cases := map[string]error{
		"memory-full": errMemoryStoreFull,
		"transport":   errors.New("idempotency: Redis acquire: connection refused"),
	}
	for name, acquireErr := range cases {
		store := &failingStore{acquireErr: acquireErr}
		p := initializedPlugin(t, nil, WithStore(store))
		response := perform(engineFor(p, route, noContent), "store-error-"+name, "")
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503 idempotency_unavailable", name, response.Code)
		}
		if store.completed.Load() != 0 || store.released.Load() != 0 {
			t.Fatalf("%s: complete=%d release=%d, want no cleanup for a reservation never acquired", name, store.completed.Load(), store.released.Load())
		}
	}
}

// failingStore returns a configured error from Acquire and records any cleanup
// the middleware attempts afterwards.
type failingStore struct {
	acquireErr error
	completed  atomic.Int32
	released   atomic.Int32
}

func (s *failingStore) Acquire(context.Context, string, string, string, time.Duration) (AcquireResult, error) {
	return AcquireResult{}, s.acquireErr
}
func (s *failingStore) Complete(context.Context, string, string, string, Response, time.Duration) error {
	s.completed.Add(1)
	return nil
}
func (s *failingStore) Release(context.Context, string, string, string) error {
	s.released.Add(1)
	return nil
}
