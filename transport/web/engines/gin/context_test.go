package gin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ginlib "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
)

// newTestRequestContext builds a requestContext over a detached gin.Context
// writing into recorder, for the methods that need no routing.
func newTestRequestContext(t *testing.T, recorder *httptest.ResponseRecorder) *requestContext {
	t.Helper()
	ginlib.SetMode(ginlib.TestMode)
	c, _ := ginlib.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	return newRequestContext(c)
}

// TestStatusRecordsWithoutCommitting pins the half of the Status contract that
// a naive forward to WriteHeader would break. Nine "do not write this response
// twice" guards read Written(), so a Status that committed immediately would
// make every one of them fire against a response body that had not been
// written yet.
func TestStatusRecordsWithoutCommitting(t *testing.T) {
	recorder := httptest.NewRecorder()
	rc := newTestRequestContext(t, recorder)

	rc.Status(http.StatusTeapot)

	assert.False(t, rc.Writer().Written(), "Status 只应记录状态码，不得提交响应")
	assert.Equal(t, http.StatusTeapot, rc.Writer().Status(), "Status 应已记录状态码")
	assert.Equal(t, http.StatusOK, recorder.Code, "底层 recorder 不应收到任何已提交的状态行")
}

// TestSetWriterRoundTripsThroughTheShim pins that a neutral writer installed
// through SetWriter is the writer gin actually renders into. Without the
// round trip, middleware that swaps in a buffering or recording writer would
// see nothing: gin would keep writing through the writer it already held.
func TestSetWriterRoundTripsThroughTheShim(t *testing.T) {
	recorder := httptest.NewRecorder()
	rc := newTestRequestContext(t, recorder)
	base := newRecordingWriter(httptest.NewRecorder())

	rc.SetWriter(base)

	require.False(t, base.Written(), "安装时不应提交任何内容")
	require.NoError(t, rc.JSON(http.StatusCreated, map[string]string{"name": "xbc"}))

	assert.True(t, base.Written(), "gin 的渲染必须落到 SetWriter 安装的中立写入器上")
	assert.Equal(t, http.StatusCreated, base.Status(), "状态码应经 shim 到达中立写入器")
	assert.JSONEq(t, `{"name":"xbc"}`, base.recorder.Body.String(), "响应体应写入中立写入器")
	assert.Equal(t, http.StatusOK, recorder.Code, "原写入器不应再收到任何内容")

	assert.Same(t, rc.Writer(), rc.c.Writer, "Writer 必须返回 gin 当前持有的写入器")
}

// TestStatusRecordedThroughAReplacedWriterSurvivesTheRestore pins that the
// request has exactly one pending-status holder, gin's own writer at the
// bottom of the chain. gzip, timeout, and idempotency all install a wrapper,
// let the handler run, and then restore the writer they replaced; a status
// recorded into a holder that lives only as long as the wrapper is lost at
// that restore, and the response goes out with the default 200 instead.
func TestStatusRecordedThroughAReplacedWriterSurvivesTheRestore(t *testing.T) {
	recorder := httptest.NewRecorder()
	rc := newTestRequestContext(t, recorder)
	original := rc.Writer()
	wrapper := &passthroughWriter{ResponseWriter: original}

	rc.SetWriter(wrapper)
	rc.Status(http.StatusAccepted)

	assert.False(t, wrapper.Written(), "Status 仍只应记录，不得提交响应")
	assert.Equal(t, http.StatusAccepted, wrapper.Status(),
		"包装器读到的状态码应是处理器记录的那个，而不是默认值")

	rc.SetWriter(original)
	rc.c.Writer.WriteHeaderNow()

	assert.Equal(t, http.StatusAccepted, recorder.Code, "恢复原写入器后，记录的状态码仍应被提交")
}

// passthroughWriter is the minimal shape the buffering middleware share: it
// embeds the writer it replaced and forwards everything, so Status and Written
// are answered by the holder beneath it.
type passthroughWriter struct {
	web.ResponseWriter
}

// Unwrap is what every real buffering wrapper carries, and it is part of the
// shape rather than an extra: http.ResponseController walks it to reach the
// connection, so a stand-in without it would model a wrapper no middleware in
// this repository actually is.
func (w *passthroughWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// TestJSONReportsRenderFailure pins the return value the neutral signature
// adds. gin's own JSON returns nothing and buries a render failure in
// Context.Errors, so a forward that always returned nil would compile, pass a
// happy-path test, and silently claim success on every failed render.
func TestJSONReportsRenderFailure(t *testing.T) {
	recorder := httptest.NewRecorder()
	rc := newTestRequestContext(t, recorder)

	err := rc.JSON(http.StatusOK, make(chan int))

	require.Error(t, err, "渲染失败必须报告给调用方")
	var unsupported *json.UnsupportedTypeError
	assert.ErrorAs(t, err, &unsupported, "应原样报告引擎的渲染错误")
}

// TestJSONIgnoresErrorsRecordedBeforeTheRender pins that JSON reports on its
// own render, not on whatever the chain has already accumulated. gin's Errors
// is a shared per-request slice that third-party middleware appends to through
// Context.Error, so an implementation testing len(Errors) > 0 instead of the
// before/after delta would turn every successful render that follows such a
// middleware into a reported failure, which the error boundary then renders as
// a 500.
func TestJSONIgnoresErrorsRecordedBeforeTheRender(t *testing.T) {
	recorder := httptest.NewRecorder()
	rc := newTestRequestContext(t, recorder)
	//nolint:errcheck // Error returns the entry it recorded; only the side effect matters here.
	rc.c.Error(errors.New("recorded by earlier middleware"))

	require.NoError(t, rc.JSON(http.StatusOK, map[string]string{"name": "xbc"}),
		"渲染成功时不得因链上已有的错误而误报失败")
	assert.JSONEq(t, `{"name":"xbc"}`, recorder.Body.String(), "响应体应正常写出")
}

// TestBindNegotiatesTheContentType pins that Bind forwards to the engine's
// content-negotiating entry point rather than to a single-format one. Binding
// is exactly the kind of work the neutral face delegates instead of
// reimplementing, and a forward to ShouldBindJSON would satisfy the signature,
// pass every JSON test, and silently break form, XML, and multipart requests.
func TestBindNegotiatesTheContentType(t *testing.T) {
	recorder := httptest.NewRecorder()
	rc := newTestRequestContext(t, recorder)
	rc.SetRequest(httptest.NewRequest(http.MethodPost, "/", strings.NewReader("name=xbc")))
	rc.Request().Header.Set("Content-Type", "application/x-www-form-urlencoded")

	var payload struct {
		Name string `form:"name"`
	}
	require.NoError(t, rc.Bind(&payload), "表单编码的请求体必须能绑定：Bind 应交给引擎自己的内容协商")
	assert.Equal(t, "xbc", payload.Name, "Bind 应按 Content-Type 选择表单绑定器")
}

// TestBindReturnsTheEngineErrorUnwrapped pins that binding policy stays in
// transport/web. ParamError turns a binding failure into a safe Problem
// Detail, and doing that here would give every engine adapter its own copy of
// a decision xbc makes once. The assertion is a direct type assertion rather
// than errors.As precisely so that a ParamError wrapper could not satisfy it.
func TestBindReturnsTheEngineErrorUnwrapped(t *testing.T) {
	recorder := httptest.NewRecorder()
	rc := newTestRequestContext(t, recorder)
	rc.SetRequest(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":42}`)))
	rc.Request().Header.Set("Content-Type", "application/json")

	var payload struct {
		Name string `json:"name"`
	}
	err := rc.Bind(&payload)

	require.Error(t, err, "类型不匹配的请求体应绑定失败")
	_, native := err.(*json.UnmarshalTypeError)
	assert.True(t, native, "Bind 必须原样返回引擎的绑定错误，包装成 ParamError 是 web 的职责")
}

// TestBindURIReadsRouteParameters pins that URI binding runs against the
// engine's own parameter table, which only exists on a routed request.
func TestBindURIReadsRouteParameters(t *testing.T) {
	ginlib.SetMode(ginlib.TestMode)
	engine := ginlib.New()

	var bound struct {
		ID string `uri:"id"`
	}
	var bindErr error
	var param string
	engine.GET("/items/:id", func(c *ginlib.Context) {
		rc := newRequestContext(c)
		param = rc.Param("id")
		bindErr = rc.BindURI(&bound)
	})

	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/items/42", nil))

	require.NoError(t, bindErr, "BindURI 不应返回错误")
	assert.Equal(t, "42", param, "Param 应读到路由通配段")
	assert.Equal(t, "42", bound.ID, "BindURI 应填充 uri 标签字段")
}

// TestClientIPForwardsToTheEngine pins the doc comment's claim that ClientIP
// is the engine's answer. With no trusted proxy the forwarded header must be
// ignored, which is a decision SetTrustedProxies made inside gin; anything
// derived here from the request alone would have to re-decide it and could
// disagree.
func TestClientIPForwardsToTheEngine(t *testing.T) {
	ginlib.SetMode(ginlib.TestMode)
	engine := ginlib.New()
	require.NoError(t, engine.SetTrustedProxies(nil), "SetTrustedProxies(nil) 不应失败")

	var got string
	engine.GET("/ip", func(c *ginlib.Context) { got = newRequestContext(c).ClientIP() })

	request := httptest.NewRequest(http.MethodGet, "/ip", nil)
	request.RemoteAddr = "203.0.113.7:1234"
	request.Header.Set("X-Forwarded-For", "198.51.100.9")
	engine.ServeHTTP(httptest.NewRecorder(), request)

	assert.Equal(t, "203.0.113.7", got,
		"无受信代理时 ClientIP 必须给出引擎自己的答案，忽略 X-Forwarded-For")
}

// TestClientIPHonoursTrustedProxies is the discriminating half of the ClientIP
// contract. The untrusted case above does not discriminate: there the engine
// ignores X-Forwarded-For and falls back to RemoteAddr, which is exactly what a
// hand-rolled net.SplitHostPort(RemoteAddr) also returns. Only the trusted case
// separates them -- the engine consults the header it was configured to trust,
// while any self-derived answer keeps reporting the proxy's own address. That
// divergence is not cosmetic: ClientIP keys rate limiting, so an implementation
// that silently ignored trusted-proxy configuration would collapse every client
// behind the proxy into a single bucket.
func TestClientIPHonoursTrustedProxies(t *testing.T) {
	ginlib.SetMode(ginlib.TestMode)
	engine := ginlib.New()
	require.NoError(t, engine.SetTrustedProxies([]string{"127.0.0.1"}))

	var observed string
	handle := web.Handle(func(_ context.Context, c *web.Ctx) error {
		observed = c.ClientIP()
		c.Status(http.StatusNoContent)
		return nil
	})
	engine.GET("/whoami", func(c *ginlib.Context) { handle(ctxFor(c)) })

	call := func(remoteAddr string) string {
		request := httptest.NewRequest(http.MethodGet, "/whoami", nil)
		request.RemoteAddr = remoteAddr
		request.Header.Set("X-Forwarded-For", "9.9.9.9")
		engine.ServeHTTP(httptest.NewRecorder(), request)
		return observed
	}

	assert.Equal(t, "9.9.9.9", call("127.0.0.1:51234"),
		"请求来自受信代理时必须采信 X-Forwarded-For，这是只有真正转发给引擎才能得到的答案")
	assert.Equal(t, "203.0.113.7", call("203.0.113.7:51234"),
		"请求来自非受信代理时必须忽略 X-Forwarded-For，与引擎自身的判断一致")
}

// TestAbortStopsLaterHandlers pins that Abort moves the engine's own handler
// index. A no-op Abort, or one that only set adapter-local state, would leave
// every aborting middleware in the codebase running the chain it meant to
// stop.
func TestAbortStopsLaterHandlers(t *testing.T) {
	ginlib.SetMode(ginlib.TestMode)
	engine := ginlib.New()

	var ran []string
	engine.GET("/abort",
		func(c *ginlib.Context) {
			ran = append(ran, "first")
			newRequestContext(c).Abort()
		},
		func(c *ginlib.Context) { ran = append(ran, "second") },
	)

	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/abort", nil))

	assert.Equal(t, []string{"first"}, ran, "Abort 之后的处理器不得再运行")
}

// TestNextSuspendsUntilTheRestOfTheChainHasRun pins the suspend-and-resume
// shape of Next rather than merely that the later handler ran. A Next that
// returned immediately would still let the chain finish, but the recorded
// order would no longer bracket the inner handler.
func TestNextSuspendsUntilTheRestOfTheChainHasRun(t *testing.T) {
	ginlib.SetMode(ginlib.TestMode)
	engine := ginlib.New()

	var ran []string
	engine.GET("/next",
		func(c *ginlib.Context) {
			ran = append(ran, "before")
			newRequestContext(c).Next()
			ran = append(ran, "after")
		},
		func(c *ginlib.Context) { ran = append(ran, "inner") },
	)

	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/next", nil))

	assert.Equal(t, []string{"before", "inner", "after"}, ran,
		"Next 必须在链的其余部分跑完后才返回")
}

// TestSetRequestIsVisibleThroughRequest pins the pairing the rest of the
// framework depends on: SetContext publishes a context by rewriting the
// request, and every downstream reader finds it through Request.
func TestSetRequestIsVisibleThroughRequest(t *testing.T) {
	recorder := httptest.NewRecorder()
	rc := newTestRequestContext(t, recorder)

	replacement := httptest.NewRequest(http.MethodPut, "/replaced", nil)
	rc.SetRequest(replacement)

	assert.Same(t, replacement, rc.Request(), "Request 必须返回 SetRequest 安装的请求")
	assert.Same(t, replacement, rc.c.Request, "SetRequest 必须改写引擎持有的请求")
}
