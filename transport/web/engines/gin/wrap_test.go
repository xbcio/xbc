package gin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	ginlib "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

// newTestEngine builds the adapter's engine and returns both the neutral port
// and the gin engine underneath, which is what a test serves requests through.
func newTestEngine(t *testing.T) (web.Engine, *ginlib.Engine) {
	t.Helper()
	ginlib.SetMode(ginlib.TestMode)
	e, err := Factory{}.NewEngine(web.Options{})
	require.NoError(t, err)
	return e, e.(*engine).e
}

// TestWrapRunsThirdPartyMiddlewareAndPropagatesGinControlFlow is
// TestWrapGinHandlerRunsThirdPartyMiddlewareAndPropagatesGinControlFlow's
// migration from transport/web/context_test.go: web.WrapGinHandler relocated
// here as Wrap when the Engine port landed (Ruling 4 / design §7.3).
func TestWrapRunsThirdPartyMiddlewareAndPropagatesGinControlFlow(t *testing.T) {
	port, ginEngine := newTestEngine(t)

	var thirdPartyRan bool
	thirdParty := ginlib.HandlerFunc(func(c *ginlib.Context) {
		thirdPartyRan = true
		c.Header("X-Wrapped", "true")
		c.Next()
	})

	mw := Wrap(thirdParty, web.Order{})
	port.Handle(http.MethodGet, "/wrapped", []web.Handler{
		mw.Handler(),
		func(_ context.Context, c *web.Ctx) error {
			c.Status(http.StatusOK)
			return nil
		},
	})

	response := httptest.NewRecorder()
	ginEngine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/wrapped", nil))

	assert.True(t, thirdPartyRan)
	assert.Equal(t, "true", response.Header().Get("X-Wrapped"))
	assert.Equal(t, http.StatusOK, response.Code)
}

// TestWrapReportsErrorsTheMiddlewareRecordedDuringItsOwnCall pins the seam
// between gin's error accumulator and xbc's error boundary. A gin-native
// middleware reports failure by pushing onto Context.Errors instead of
// returning, and such middleware can only enter an xbc chain through Wrap, so
// draining it here is what keeps the failure from vanishing.
func TestWrapReportsErrorsTheMiddlewareRecordedDuringItsOwnCall(t *testing.T) {
	port, ginEngine := newTestEngine(t)

	recorded := errors.New("recorded by the third-party middleware")
	//nolint:errcheck // Error returns the entry it recorded; only the side effect matters here.
	thirdParty := ginlib.HandlerFunc(func(c *ginlib.Context) { c.Error(recorded) })

	var reported error
	port.Handle(http.MethodGet, "/reported", []web.Handler{
		func(_ context.Context, c *web.Ctx) error {
			c.Next()
			return nil
		},
		Wrap(thirdParty, web.Order{}).Handler(),
	})
	// The boundary that would normally render this error is transport/web's;
	// capturing it directly keeps this test about the drain and nothing else.
	port.Handle(http.MethodGet, "/captured", []web.Handler{
		func(_ context.Context, c *web.Ctx) error {
			reported = Wrap(thirdParty, web.Order{}).Handler()(context.Background(), c)
			return nil
		},
	})

	ginEngine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/captured", nil))

	assert.ErrorIs(t, reported, recorded, "an error the middleware recorded during its own call must be reported out")
}

// TestWrapReportedErrorsAreRenderedThroughTheErrorBoundary is the end-to-end
// half of the same seam, rebuilt here from TestGinReportedErrorsAreJoinedBeforeMapping,
// which lived in transport/web/errors_test.go while that package still owned
// gin. The test above proves only that Handler() returns the error; the whole
// path from gin's accumulator through the error boundary to a rendered Problem
// Detail could break with it still green, so the judgement here is the
// response, never the returned error.
//
// Two errors are pushed on purpose, and only the first wraps the domain error
// the mapper recognizes. Joining them is what keeps errors.Is reachable
// through both: an implementation reporting only the last entry would miss the
// mapper and fall through to the generic 500.
func TestWrapReportedErrorsAreRenderedThroughTheErrorBoundary(t *testing.T) {
	port, ginEngine := newTestEngine(t)

	domainErr := errors.New("domain failure")
	mapper := web.ErrorMapperFunc(func(_ *web.Ctx, err error) (web.ProblemDetail, bool) {
		if errors.Is(err, domainErr) {
			return web.NewProblem(http.StatusUnprocessableEntity, "domain_failure"), true
		}
		return web.ProblemDetail{}, false
	})
	thirdParty := ginlib.HandlerFunc(func(c *ginlib.Context) {
		//nolint:errcheck // Error returns the entry it recorded; only the side effect matters here.
		c.Error(fmt.Errorf("first: %w", domainErr))
		//nolint:errcheck // Same: the unrelated second entry is pushed for its side effect.
		c.Error(errors.New("unrelated later failure"))
		c.Abort()
	})

	port.Handle(http.MethodGet, "/reported", []web.Handler{
		web.OnError(mapper),
		Wrap(thirdParty, web.Order{}).Handler(),
	})

	response := httptest.NewRecorder()
	ginEngine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/reported", nil))

	require.Equal(t, http.StatusUnprocessableEntity, response.Code,
		"an error reported by a gin-native middleware must be mapped through the error boundary and rendered")
	assert.Equal(t, "domain_failure", decodeProblem(t, response).Properties["code"],
		"the join must keep errors.Is reachable through every recorded entry -- otherwise an implementation that reports only the last error would pass too")
}

// TestWrapReportsErrorsEvenAfterTheResponseIsCommitted covers the half of the
// seam the two tests above leave open. A gin-native middleware that has already
// sent a response and only then records a failure -- a streaming or
// partial-write path, typically -- must not have that failure discarded here:
// dropping it would make an error vanish on precisely the requests where
// something already went out the door, and asymmetrically at that, since the
// same failure reported by return value is logged.
//
// What the client receives is the other half of the judgement. transport/web's
// boundary logs a failure it can no longer render and leaves the committed
// response untouched, so the internal detail stays in the log instead of
// following the error out to the caller.
func TestWrapReportsErrorsEvenAfterTheResponseIsCommitted(t *testing.T) {
	port, ginEngine := newTestEngine(t)

	recorded := errors.New("internal detail the caller must never see")
	thirdParty := ginlib.HandlerFunc(func(c *ginlib.Context) {
		c.String(http.StatusOK, "already sent")
		//nolint:errcheck // Error returns the entry it recorded; only the side effect matters here.
		c.Error(recorded)
	})
	// A mapper that claims every error makes the "response left alone" half
	// real: without one, an implementation that rewrote the committed response
	// would still answer 200 and look correct here.
	mapper := web.ErrorMapperFunc(func(*web.Ctx, error) (web.ProblemDetail, bool) {
		return web.NewProblem(http.StatusInternalServerError, "mapped_after_commit"), true
	})

	var reported error
	var committed bool
	port.Handle(http.MethodGet, "/committed", []web.Handler{
		web.OnError(mapper),
		func(ctx context.Context, c *web.Ctx) error {
			reported = Wrap(thirdParty, web.Order{}).Handler()(ctx, c)
			committed = c.Writer().Written()
			// Returning it is what a Wrap-registered middleware does on its
			// own; capturing it as well keeps the drain itself observable.
			return reported
		},
	})

	response := httptest.NewRecorder()
	ginEngine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/committed", nil))

	require.True(t, committed, "precondition unmet: the response must really be committed, otherwise this test never reached the branch it is about")
	assert.ErrorIs(t, reported, recorded, "a committed response is no reason to discard the error -- it must still be reported to the error boundary")
	assert.Equal(t, http.StatusOK, response.Code, "a committed response must not be rewritten")
	assert.Equal(t, "already sent", response.Body.String())
	assert.NotContains(t, response.Body.String(), recorded.Error(), "the internal error detail belongs in the log only and must not be returned to the caller")
}

// TestWrapIgnoresErrorsRecordedBeforeItRan pins the before/after delta.
// Context.Errors is a shared per-request slice, so reporting everything in it
// would attribute an earlier middleware's failure to this one and render it a
// second time.
func TestWrapIgnoresErrorsRecordedBeforeItRan(t *testing.T) {
	port, ginEngine := newTestEngine(t)

	var reported error
	port.Handle(http.MethodGet, "/earlier", []web.Handler{
		func(_ context.Context, c *web.Ctx) error {
			//nolint:errcheck // Only the recorded entry matters here.
			FromCtx(c).Error(errors.New("recorded by an earlier middleware"))
			reported = Wrap(ginlib.HandlerFunc(func(*ginlib.Context) {}), web.Order{}).Handler()(context.Background(), c)
			return nil
		},
	})

	ginEngine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/earlier", nil))

	assert.NoError(t, reported, "an error recorded earlier in the chain does not belong to this middleware and must not be reported by it")
}

// TestWrapRefusesToRunOnAnotherEngine pins Ruling 26: a gin-native middleware
// that quietly does not run is an authentication check, a rate limit, or a
// tracing span that has disappeared without a trace, so the adapter refuses
// loudly instead of skipping.
func TestWrapRefusesToRunOnAnotherEngine(t *testing.T) {
	var thirdPartyRan bool
	mw := Wrap(ginlib.HandlerFunc(func(*ginlib.Context) { thirdPartyRan = true }), web.Order{})

	c := enginetest.NewCtx(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	err := mw.Handler()(context.Background(), c)

	require.Error(t, err, "a request gin is not serving must raise an error rather than be skipped silently")
	assert.ErrorIs(t, err, errNotGinEngine)
	assert.False(t, thirdPartyRan, "a wrapped gin middleware must not run on another engine")
}

func TestWrapPanicsOnNilHandler(t *testing.T) {
	assert.PanicsWithValue(t, "xbc: gin.Wrap requires a non-nil handler", func() {
		Wrap(nil, web.Order{})
	})
}

func TestWrapOrderReturnsConfiguredOrder(t *testing.T) {
	mw := Wrap(ginlib.HandlerFunc(func(*ginlib.Context) {}), web.Order{Phase: web.PhaseSecurity})
	assert.Equal(t, web.PhaseSecurity, mw.Order().Phase)
}

func TestFromCtxReturnsUnderlyingGinContext(t *testing.T) {
	ginlib.SetMode(ginlib.TestMode)
	gc, _ := ginlib.CreateTestContext(httptest.NewRecorder())
	c := web.NewCtx(newRequestContext(gc))
	assert.Same(t, gc, FromCtx(c))
}

func TestFromCtxReturnsNilForNilCtx(t *testing.T) {
	assert.Nil(t, FromCtx(nil))
}

// TestFromCtxReturnsNilForAnotherEngine pins that the escape hatch cannot
// manufacture a *gin.Context for a request gin never saw.
func TestFromCtxReturnsNilForAnotherEngine(t *testing.T) {
	c := enginetest.NewCtx(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Nil(t, FromCtx(c))
}
