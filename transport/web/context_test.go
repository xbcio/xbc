package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCtxDoesNotImplementContextContext is the core invariant of this
// design: Ctx must never satisfy context.Context. A previous design embedded
// context.Context inside the request object, which let code like
// db.WithContext(ctx) compile against a value that also carried a live
// *gin.Context, silently re-merging the two things this split is meant to
// keep apart. Ctx intentionally has no Deadline, Done, or Err methods for
// exactly this reason. If a future change adds one of those methods, this
// test goes red -- read that as "the ctx/Ctx separation just broke," not as
// "add the missing method to make the assertion pass."
func TestCtxDoesNotImplementContextContext(t *testing.T) {
	ctxType := reflect.TypeOf((*Ctx)(nil))
	contextInterfaceType := reflect.TypeOf((*context.Context)(nil)).Elem()

	if ctxType.Implements(contextInterfaceType) {
		t.Fatalf("*Ctx must not implement context.Context, but it does")
	}
}

func TestHandlePassesRequestContextAndReusesErrorBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()

	type requestContextKey struct{}
	var observedCtx context.Context
	engine.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), requestContextKey{}, "marker"))
		c.Next()
	})
	engine.GET("/ok", Handle(func(ctx context.Context, c *Ctx) error {
		observedCtx = ctx
		c.JSON(http.StatusOK, map[string]string{"status": "ok"})
		return nil
	}))
	engine.GET("/fail", Handle(func(ctx context.Context, c *Ctx) error {
		return errFromHandleTest
	}))

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
		Handle(nil)
	})
}

func TestCtxBindAdaptsGinBindingErrorsLikeParamError(t *testing.T) {
	type bindRequest struct {
		Name string `json:"name" binding:"required"`
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/requests", Handle(func(_ context.Context, c *Ctx) error {
		var request bindRequest
		if err := c.Bind(&request); err != nil {
			return err
		}
		c.Status(http.StatusNoContent)
		return nil
	}))

	okResponse := httptest.NewRecorder()
	okRequest := httptest.NewRequest(http.MethodPost, "/requests", strings.NewReader(`{"name":"Alice"}`))
	okRequest.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(okResponse, okRequest)
	assert.Equal(t, http.StatusNoContent, okResponse.Code)

	badResponse := httptest.NewRecorder()
	badRequest := httptest.NewRequest(http.MethodPost, "/requests", strings.NewReader(`{}`))
	badRequest.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(badResponse, badRequest)
	assert.Equal(t, http.StatusBadRequest, badResponse.Code)
	problem := decodeProblem(t, badResponse)
	assert.Equal(t, "validation_failed", problem.Properties["code"])
}

func TestCtxBindURIAdaptsGinBindingErrors(t *testing.T) {
	type uriRequest struct {
		ID int `uri:"id" binding:"required"`
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/items/:id", Handle(func(_ context.Context, c *Ctx) error {
		var request uriRequest
		if err := c.BindURI(&request); err != nil {
			return err
		}
		c.JSON(http.StatusOK, request)
		return nil
	}))

	okResponse := httptest.NewRecorder()
	engine.ServeHTTP(okResponse, httptest.NewRequest(http.MethodGet, "/items/42", nil))
	assert.Equal(t, http.StatusOK, okResponse.Code)

	badResponse := httptest.NewRecorder()
	engine.ServeHTTP(badResponse, httptest.NewRequest(http.MethodGet, "/items/", nil))
	assert.Equal(t, http.StatusNotFound, badResponse.Code)
}

func TestCtxRequestReadingMethods(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/items/:id", Handle(func(_ context.Context, c *Ctx) error {
		c.JSON(http.StatusOK, map[string]string{
			"id":     c.Param("id"),
			"filter": c.Query("filter"),
			"sort":   c.DefaultQuery("sort", "created"),
			"agent":  c.GetHeader("X-Test-Agent"),
		})
		return nil
	}))

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
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/status", Handle(func(_ context.Context, c *Ctx) error {
		c.SetHeader("X-Custom", "value")
		c.Status(http.StatusAccepted)
		return nil
	}))
	engine.GET("/string", Handle(func(_ context.Context, c *Ctx) error {
		c.String(http.StatusOK, "hello %s", "world")
		return nil
	}))
	engine.GET("/data", Handle(func(_ context.Context, c *Ctx) error {
		c.Data(http.StatusOK, "application/octet-stream", []byte{1, 2, 3})
		return nil
	}))

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
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		newCtx(c).Set("trace", "abc123")
		c.Next()
	})
	engine.GET("/values", Handle(func(_ context.Context, c *Ctx) error {
		value, ok := c.Get("trace")
		if !ok {
			t.Fatalf("expected value stored by upstream middleware to be visible")
		}
		c.JSON(http.StatusOK, map[string]any{"trace": value})
		return nil
	}))

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/values", nil))
	assert.Equal(t, http.StatusOK, response.Code)
	assert.Contains(t, response.Body.String(), "abc123")

	_, missing := newCtx(gin.CreateTestContextOnly(httptest.NewRecorder(), gin.New())).Get("absent")
	assert.False(t, missing)
}

func TestCtxPrincipalReusesCurrentPrincipal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		if !SetPrincipal(newCtx(c), Principal{Subject: "alice", AuthMethod: "jwt"}) {
			t.Fatalf("expected SetPrincipal to accept a valid principal")
		}
		c.Next()
	})
	engine.GET("/whoami", Handle(func(_ context.Context, c *Ctx) error {
		principal, ok := c.Principal()
		if !ok {
			t.Fatalf("expected Ctx.Principal to see the principal set upstream")
		}
		c.JSON(http.StatusOK, map[string]string{"subject": principal.Subject})
		return nil
	}))

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/whoami", nil))
	assert.Equal(t, http.StatusOK, response.Code)
	assert.Contains(t, response.Body.String(), "alice")

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	_, ok := newCtx(c).Principal()
	assert.False(t, ok)
}

func TestCtxAbortStopsRemainingHandlers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	nextRan := false
	engine.GET("/abort",
		Handle(func(_ context.Context, c *Ctx) error {
			c.String(http.StatusTeapot, "stopped")
			c.Abort()
			return nil
		}),
		func(c *gin.Context) {
			nextRan = true
		},
	)

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/abort", nil))
	assert.Equal(t, http.StatusTeapot, response.Code)
	assert.False(t, nextRan)
}

func TestCtxEscapeHatchesExposeUnderlyingGinContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/echo", Handle(func(_ context.Context, c *Ctx) error {
		if c.Request() != c.Gin().Request {
			t.Fatalf("expected Ctx.Request to return the same *http.Request as Ctx.Gin().Request")
		}
		if c.Writer() != c.Gin().Writer {
			t.Fatalf("expected Ctx.Writer to return the same gin.ResponseWriter as Ctx.Gin().Writer")
		}
		c.Gin().JSON(http.StatusOK, map[string]string{"method": c.Request().Method})
		return nil
	}))

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/echo", nil))
	assert.Equal(t, http.StatusOK, response.Code)
	assert.Contains(t, response.Body.String(), "POST")
}

func TestWrapGinHandlerRunsThirdPartyMiddlewareAndPropagatesGinControlFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()

	var thirdPartyRan bool
	thirdParty := gin.HandlerFunc(func(c *gin.Context) {
		thirdPartyRan = true
		c.Header("X-Wrapped", "true")
		c.Next()
	})

	engine.GET("/wrapped", Handle(WrapGinHandler(thirdParty)), Handle(func(_ context.Context, c *Ctx) error {
		c.Status(http.StatusOK)
		return nil
	}))

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/wrapped", nil))

	assert.True(t, thirdPartyRan)
	assert.Equal(t, "true", response.Header().Get("X-Wrapped"))
	assert.Equal(t, http.StatusOK, response.Code)
}

func TestWrapGinHandlerPanicsOnNilHandler(t *testing.T) {
	assert.PanicsWithValue(t, "xbc: web.WrapGinHandler requires a non-nil handler", func() {
		WrapGinHandler(nil)
	})
}

// TestNewCtxWrapsTheGivenGinContextWithoutValidating pins the two properties
// plugins outside this package rely on. NewCtx must be the exact inverse of
// Ctx.Gin -- a plugin's test builds a request-carrying gin.Context and expects
// the extractor to read that very request, not some other one -- and it must
// accept a gin.Context with no request at all, because that is the state the
// nil-request guards in session and apikey exist to absorb and the only way to
// reach them from outside this package.
func TestNewCtxWrapsTheGivenGinContextWithoutValidating(t *testing.T) {
	gin.SetMode(gin.TestMode)

	gc, _ := gin.CreateTestContext(httptest.NewRecorder())
	request := httptest.NewRequest(http.MethodGet, "/orders/42", nil)
	request.Header.Set("X-Probe", "present")
	gc.Request = request

	c := NewCtx(gc)
	require.NotNil(t, c)
	assert.Same(t, gc, c.Gin())
	assert.Same(t, request, c.Request())
	assert.Equal(t, "present", c.GetHeader("X-Probe"))

	noRequest, _ := gin.CreateTestContext(httptest.NewRecorder())
	assert.Nil(t, NewCtx(noRequest).Request())
}

// TestCtxNextRunsDownstreamHandlerBeforeReturning pins that Next suspends the
// current handler until the rest of the chain has run. A Next that merely
// returned would still let gin run the downstream handler -- just afterwards --
// so only the interleaving distinguishes a real Next from a no-op.
func TestCtxNextRunsDownstreamHandlerBeforeReturning(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()

	var order []string
	engine.Use(Handle(func(_ context.Context, c *Ctx) error {
		order = append(order, "outer-pre")
		c.Next()
		order = append(order, "outer-post")
		return nil
	}))
	engine.GET("/", func(c *gin.Context) {
		order = append(order, "inner")
		c.Status(http.StatusNoContent)
	})

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, []string{"outer-pre", "inner", "outer-post"}, order)
}

// TestCtxSetContextPublishesThroughTheRequest pins that SetContext rewrites the
// request rather than storing a context on Ctx. The single source of truth must
// stay the request itself: a downstream third-party gin handler reads
// c.Request.Context() and would silently see the old value otherwise.
func TestCtxSetContextPublishesThroughTheRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()

	type markerKey struct{}
	var seenByCtx, seenByGin any

	engine.Use(Handle(func(ctx context.Context, c *Ctx) error {
		c.SetContext(context.WithValue(ctx, markerKey{}, "published"))
		c.Next()
		return nil
	}))
	engine.Use(Handle(func(_ context.Context, c *Ctx) error {
		seenByCtx = c.Request().Context().Value(markerKey{})
		c.Next()
		return nil
	}))
	engine.GET("/", func(c *gin.Context) {
		seenByGin = c.Request.Context().Value(markerKey{})
		c.Status(http.StatusNoContent)
	})

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, "published", seenByCtx)
	assert.Equal(t, "published", seenByGin)
}
