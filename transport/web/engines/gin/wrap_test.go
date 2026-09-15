package gin

import (
	"context"
	"errors"
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

	assert.ErrorIs(t, reported, recorded, "中间件在自身调用期间记录的错误必须被报告出来")
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

	assert.NoError(t, reported, "链上早先记录的错误不属于这个中间件，不得由它报告")
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

	require.Error(t, err, "请求不由 gin 提供服务时必须报错，而不是静默跳过")
	assert.ErrorIs(t, err, errNotGinEngine)
	assert.False(t, thirdPartyRan, "被包装的 gin 中间件不得在其他引擎上运行")
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
