package gin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	ginlib "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/xbcio/xbc/transport/web"
)

// TestWrapRunsThirdPartyMiddlewareAndPropagatesGinControlFlow is
// TestWrapGinHandlerRunsThirdPartyMiddlewareAndPropagatesGinControlFlow's
// migration from transport/web/context_test.go: web.WrapGinHandler relocated
// here as Wrap when the Engine port landed (Ruling 4 / design §7.3).
func TestWrapRunsThirdPartyMiddlewareAndPropagatesGinControlFlow(t *testing.T) {
	ginlib.SetMode(ginlib.TestMode)
	engine := ginlib.New()

	var thirdPartyRan bool
	thirdParty := ginlib.HandlerFunc(func(c *ginlib.Context) {
		thirdPartyRan = true
		c.Header("X-Wrapped", "true")
		c.Next()
	})

	mw := Wrap(thirdParty, web.Order{})
	engine.GET("/wrapped", web.Handle(mw.Handler()), web.Handle(func(_ context.Context, c *web.Ctx) error {
		c.Status(http.StatusOK)
		return nil
	}))

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/wrapped", nil))

	assert.True(t, thirdPartyRan)
	assert.Equal(t, "true", response.Header().Get("X-Wrapped"))
	assert.Equal(t, http.StatusOK, response.Code)
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
	c := web.NewCtx(gc)
	assert.Same(t, gc, FromCtx(c))
}

func TestFromCtxReturnsNilForNilCtx(t *testing.T) {
	assert.Nil(t, FromCtx(nil))
}
