package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

// TestCtxDoesNotImplementContextContext is the core invariant of this
// design: Ctx must never satisfy context.Context. A previous design embedded
// context.Context inside the request object, which let code like
// db.WithContext(ctx) compile against a value that also carried a live
// request, silently re-merging the two things this split is meant to keep
// apart. Ctx intentionally has no Deadline, Done, or Err methods for exactly
// this reason. If a future change adds one of those methods, this test goes
// red -- read that as "the ctx/Ctx separation just broke," not as "add the
// missing method to make the assertion pass."
func TestCtxDoesNotImplementContextContext(t *testing.T) {
	ctxType := reflect.TypeOf((*web.Ctx)(nil))
	contextInterfaceType := reflect.TypeOf((*context.Context)(nil)).Elem()

	if ctxType.Implements(contextInterfaceType) {
		t.Fatalf("*Ctx must not implement context.Context, but it does")
	}
}

func TestHandlePassesRequestContextAndReusesErrorBoundary(t *testing.T) {
	type requestContextKey struct{}
	var observedCtx context.Context

	engine := enginetest.New()
	engine.Use(func(ctx context.Context, c *web.Ctx) error {
		c.SetContext(context.WithValue(ctx, requestContextKey{}, "marker"))
		c.Next()
		return nil
	})
	// Handle is the adapter an engine drives its chain with, so it is invoked
	// here the way an adapter invokes it: on the request's single shared Ctx.
	ok := web.Handle(func(ctx context.Context, c *web.Ctx) error {
		observedCtx = ctx
		c.JSON(http.StatusOK, map[string]string{"status": "ok"})
		return nil
	})
	fail := web.Handle(func(context.Context, *web.Ctx) error {
		return errFromHandleTest
	})
	engine.GET("/ok", func(_ context.Context, c *web.Ctx) error { ok(c); return nil })
	engine.GET("/fail", func(_ context.Context, c *web.Ctx) error { fail(c); return nil })

	okRecorder := httptest.NewRecorder()
	engine.ServeHTTP(okRecorder, httptest.NewRequest(http.MethodGet, "/ok", nil))
	assert.Equal(t, http.StatusOK, okRecorder.Code)
	require.NotNil(t, observedCtx)
	assert.Equal(t, "marker", observedCtx.Value(requestContextKey{}))

	failRecorder := httptest.NewRecorder()
	engine.ServeHTTP(failRecorder, httptest.NewRequest(http.MethodGet, "/fail", nil))
	assert.Equal(t, http.StatusInternalServerError, failRecorder.Code)
	assert.Equal(t, "internal_server_error", decodeProblem(t, failRecorder).Properties["code"])
}

var errFromHandleTest = errFromHandleTestError{}

type errFromHandleTestError struct{}

func (errFromHandleTestError) Error() string { return "handle test failure" }

func TestHandlePanicsOnNilHandler(t *testing.T) {
	assert.PanicsWithValue(t, "xbc: web.Handle requires a non-nil handler", func() {
		web.Handle(nil)
	})
}

// TestCtxBindWrapsEngineBindingFailuresInParamError pins that Ctx.Bind and
// Ctx.BindURI are policy, not forwarding. The engine reports a plain binding
// error; only the ParamError wrapping inside Ctx turns it into a client-safe
// 4xx. Drop that wrapping and the same failure reaches the error boundary as an
// unclassified error, which maps to 500 -- a caller's malformed request would be
// reported as a server fault, and the engine's own message (which can carry the
// rejected value) would be the thing that decided the status.
//
// The status is the assertion that discriminates, not the code: an
// implementation that forwarded the engine error unwrapped still renders a
// Problem Detail, just the wrong one.
func TestCtxBindWrapsEngineBindingFailuresInParamError(t *testing.T) {
	for _, tc := range []struct {
		name string
		bind func(*web.Ctx, any) error
	}{
		{"bind", func(c *web.Ctx, target any) error { return c.Bind(target) }},
		{"bind-uri", func(c *web.Ctx, target any) error { return c.BindURI(target) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := enginetest.New()
			engine.GET("/items/{id}", func(_ context.Context, c *web.Ctx) error {
				var target struct {
					ID string `json:"id" uri:"id"`
				}
				return tc.bind(c, &target)
			})

			response := httptest.NewRecorder()
			engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/items/42", nil))

			assert.Equal(t, http.StatusBadRequest, response.Code,
				"绑定失败必须经 ParamError 归类为客户端错误，未包装时会退化成 500")
			assert.Equal(t, "invalid_request", decodeProblem(t, response).Properties["code"])
		})
	}
}

func TestCtxRequestReadingMethods(t *testing.T) {
	engine := enginetest.New()
	engine.GET("/items/{id}", func(_ context.Context, c *web.Ctx) error {
		c.JSON(http.StatusOK, map[string]string{
			"id":     c.Param("id"),
			"filter": c.Query("filter"),
			"sort":   c.DefaultQuery("sort", "created"),
			"agent":  c.GetHeader("X-Test-Agent"),
		})
		return nil
	})

	request := httptest.NewRequest(http.MethodGet, "/items/7?filter=active", nil)
	request.Header.Set("X-Test-Agent", "probe")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	assert.Equal(t, http.StatusOK, response.Code)
	assert.Contains(t, response.Body.String(), `"id":"7"`)
	assert.Contains(t, response.Body.String(), `"filter":"active"`)
	assert.Contains(t, response.Body.String(), `"sort":"created"`)
	assert.Contains(t, response.Body.String(), `"agent":"probe"`)
}

func TestCtxResponseWritingMethods(t *testing.T) {
	engine := enginetest.New()
	engine.GET("/status", func(_ context.Context, c *web.Ctx) error {
		c.SetHeader("X-Custom", "value")
		c.Status(http.StatusAccepted)
		return nil
	})
	engine.GET("/string", func(_ context.Context, c *web.Ctx) error {
		c.String(http.StatusOK, "hello %s", "world")
		return nil
	})
	engine.GET("/data", func(_ context.Context, c *web.Ctx) error {
		c.Data(http.StatusOK, "application/octet-stream", []byte{1, 2, 3})
		return nil
	})

	statusResponse := httptest.NewRecorder()
	engine.ServeHTTP(statusResponse, httptest.NewRequest(http.MethodGet, "/status", nil))
	assert.Equal(t, http.StatusAccepted, statusResponse.Code)
	assert.Equal(t, "value", statusResponse.Header().Get("X-Custom"))

	stringResponse := httptest.NewRecorder()
	engine.ServeHTTP(stringResponse, httptest.NewRequest(http.MethodGet, "/string", nil))
	assert.Equal(t, "hello world", stringResponse.Body.String())

	dataResponse := httptest.NewRecorder()
	engine.ServeHTTP(dataResponse, httptest.NewRequest(http.MethodGet, "/data", nil))
	assert.Equal(t, []byte{1, 2, 3}, dataResponse.Body.Bytes())
	assert.Equal(t, "application/octet-stream", dataResponse.Header().Get("Content-Type"))
}

func TestCtxSetGetRoundTripsRequestScopedValues(t *testing.T) {
	engine := enginetest.New()
	engine.Use(func(_ context.Context, c *web.Ctx) error {
		c.Set("trace", "abc123")
		c.Next()
		return nil
	})
	engine.GET("/values", func(_ context.Context, c *web.Ctx) error {
		value, ok := c.Get("trace")
		if !ok {
			t.Fatalf("expected value stored by upstream middleware to be visible")
		}
		c.JSON(http.StatusOK, map[string]any{"trace": value})
		return nil
	})

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/values", nil))
	assert.Equal(t, http.StatusOK, response.Code)
	assert.Contains(t, response.Body.String(), "abc123")

	_, missing := enginetest.NewCtx(httptest.NewRecorder(), nil).Get("absent")
	assert.False(t, missing)
}

func TestCtxPrincipalReusesCurrentPrincipal(t *testing.T) {
	engine := enginetest.New()
	engine.Use(func(_ context.Context, c *web.Ctx) error {
		if !web.SetPrincipal(c, web.Principal{Subject: "alice", AuthMethod: "jwt"}) {
			t.Fatalf("expected SetPrincipal to accept a valid principal")
		}
		c.Next()
		return nil
	})
	engine.GET("/whoami", func(_ context.Context, c *web.Ctx) error {
		principal, ok := c.Principal()
		if !ok {
			t.Fatalf("expected Ctx.Principal to see the principal set upstream")
		}
		c.JSON(http.StatusOK, map[string]string{"subject": principal.Subject})
		return nil
	})

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/whoami", nil))
	assert.Equal(t, http.StatusOK, response.Code)
	assert.Contains(t, response.Body.String(), "alice")

	_, ok := enginetest.NewCtx(httptest.NewRecorder(), nil).Principal()
	assert.False(t, ok)
}

func TestCtxAbortStopsRemainingHandlers(t *testing.T) {
	nextRan := false
	engine := enginetest.New()
	engine.GET("/abort",
		func(_ context.Context, c *web.Ctx) error {
			c.String(http.StatusTeapot, "stopped")
			c.Abort()
			return nil
		},
		func(context.Context, *web.Ctx) error {
			nextRan = true
			return nil
		},
	)

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/abort", nil))
	assert.Equal(t, http.StatusTeapot, response.Code)
	assert.False(t, nextRan)
}

// TestNewCtxWrapsTheGivenRequestContextWithoutValidating pins the two
// properties plugins outside this package rely on. NewCtx must be the exact
// inverse of Ctx.RequestContext -- a plugin's test builds a request-carrying
// RequestContext and expects the extractor to read that very request, not some
// other one -- and it must accept a RequestContext with no request at all,
// because that is the state the nil-request guards in session and apikey exist
// to absorb and the only way to reach them from outside this package.
func TestNewCtxWrapsTheGivenRequestContextWithoutValidating(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/orders/42", nil)
	request.Header.Set("X-Probe", "present")
	rc := enginetest.NewRequestContext(httptest.NewRecorder(), request)

	c := web.NewCtx(rc)
	require.NotNil(t, c)
	assert.Same(t, rc, c.RequestContext())
	assert.Same(t, request, c.Request())
	assert.Equal(t, "present", c.GetHeader("X-Probe"))

	assert.Nil(t, web.NewCtx(enginetest.NewRequestContext(httptest.NewRecorder(), nil)).Request())
}

// TestCtxNextRunsDownstreamHandlerBeforeReturning pins that Next suspends the
// current handler until the rest of the chain has run. A Next that merely
// returned would still let the engine run the downstream handler -- just
// afterwards -- so only the interleaving distinguishes a real Next from a
// no-op.
func TestCtxNextRunsDownstreamHandlerBeforeReturning(t *testing.T) {
	var order []string

	engine := enginetest.New()
	engine.Use(func(_ context.Context, c *web.Ctx) error {
		order = append(order, "outer-pre")
		c.Next()
		order = append(order, "outer-post")
		return nil
	})
	engine.GET("/", func(_ context.Context, c *web.Ctx) error {
		order = append(order, "inner")
		c.Status(http.StatusNoContent)
		return nil
	})

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, []string{"outer-pre", "inner", "outer-post"}, order)
}

// TestCtxSetContextPublishesThroughTheRequest pins that SetContext rewrites the
// request rather than storing a context on Ctx. The single source of truth must
// stay the request itself: a downstream engine-native handler reads
// Request.Context() and would silently see the old value otherwise.
func TestCtxSetContextPublishesThroughTheRequest(t *testing.T) {
	type markerKey struct{}
	var seenByCtx, seenByRequest any

	engine := enginetest.New()
	engine.Use(func(ctx context.Context, c *web.Ctx) error {
		c.SetContext(context.WithValue(ctx, markerKey{}, "published"))
		c.Next()
		return nil
	})
	engine.Use(func(_ context.Context, c *web.Ctx) error {
		seenByCtx = c.Request().Context().Value(markerKey{})
		c.Next()
		return nil
	})
	engine.GET("/", func(_ context.Context, c *web.Ctx) error {
		seenByRequest = c.RequestContext().Request().Context().Value(markerKey{})
		c.Status(http.StatusNoContent)
		return nil
	})

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, "published", seenByCtx)
	assert.Equal(t, "published", seenByRequest)
}
