package enginetest

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
)

// TestChainRunsInRegistrationOrder pins the executor's ordering: a handler
// registered earlier runs earlier, and the global chain runs ahead of the route
// chain. Tests that migrate onto this engine rely on that order to place a
// middleware relative to the handler it wraps.
func TestChainRunsInRegistrationOrder(t *testing.T) {
	engine := New()

	var order []string
	engine.Use(func(_ context.Context, c *web.Ctx) error {
		order = append(order, "global-before")
		c.Next()
		order = append(order, "global-after")
		return nil
	})
	engine.GET("/ordered", func(_ context.Context, c *web.Ctx) error {
		order = append(order, "route")
		c.Status(http.StatusNoContent)
		return nil
	})

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ordered", nil))

	assert.Equal(t, http.StatusNoContent, recorder.Code, "路由处理器设置的状态码应写入响应")
	assert.Equal(t, []string{"global-before", "route", "global-after"}, order,
		"全局链应先于路由链进入，并在其返回后恢复")
}

// TestAbortStopsRemainingHandlers pins that Abort ends the chain rather than
// merely marking it: a handler after the aborting one must not run, and the
// aborting handler's own response must survive.
func TestAbortStopsRemainingHandlers(t *testing.T) {
	engine := New()

	downstreamRan := false
	engine.GET("/aborted",
		func(_ context.Context, c *web.Ctx) error {
			c.Status(http.StatusForbidden)
			c.Abort()
			return nil
		},
		func(_ context.Context, c *web.Ctx) error {
			downstreamRan = true
			c.Status(http.StatusOK)
			return nil
		},
	)

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/aborted", nil))

	assert.False(t, downstreamRan, "Abort 之后的处理器不得运行")
	assert.Equal(t, http.StatusForbidden, recorder.Code, "中止处理器写下的状态码应保留")
}

// TestStatusRecordsWithoutCommitting pins the half of RequestContext.Status
// that middleware depends on: recording a status must leave the response
// uncommitted, so a later middleware can still replace it. A writer that
// committed here would make every "has this response been written yet" guard in
// the framework read true too early.
func TestStatusRecordsWithoutCommitting(t *testing.T) {
	engine := New()

	engine.GET("/recorded", func(_ context.Context, c *web.Ctx) error {
		c.Status(http.StatusTeapot)
		assert.False(t, c.Writer().Written(), "Status 只应记录状态码，不得提交响应")
		assert.Equal(t, http.StatusTeapot, c.Writer().Status(), "Status 记录的状态码应可读回")
		assert.Equal(t, -1, c.Writer().Size(), "尚未写入正文时 Size 应为 -1")
		return nil
	})

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/recorded", nil))

	assert.Equal(t, http.StatusTeapot, recorder.Code,
		"链结束时记录的状态码应被提交，否则只调用 Status 的处理器不会产生响应")
}

// TestStatusRecordedThroughAReplacedWriterSurvivesTheRestore pins that the
// request has exactly one pending-status holder. gzip, timeout, and
// idempotency all install a wrapper, let the handler run, and then restore the
// writer they replaced; if installing a wrapper started a second holder, a
// handler that only calls Status would lose its status when that holder went
// away and the response would go out with the default 200 instead.
func TestStatusRecordedThroughAReplacedWriterSurvivesTheRestore(t *testing.T) {
	engine := New()

	var observed int
	engine.Use(func(_ context.Context, c *web.Ctx) error {
		original := c.Writer()
		wrapper := &passthroughWriter{ResponseWriter: original}
		c.SetWriter(wrapper)
		defer c.SetWriter(original)
		c.Next()
		observed = wrapper.Status()
		return nil
	})
	engine.GET("/wrapped", func(_ context.Context, c *web.Ctx) error {
		c.Status(http.StatusAccepted)
		return nil
	})

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/wrapped", nil))

	assert.Equal(t, http.StatusAccepted, observed,
		"包装器读到的状态码应是处理器记录的那个，而不是默认值")
	assert.Equal(t, http.StatusAccepted, recorder.Code,
		"恢复原写入器后，记录的状态码仍应被提交")
}

// passthroughWriter is the minimal shape the buffering middleware share: it
// embeds the writer it replaced and forwards everything, so Status and Written
// are answered by the holder beneath it.
type passthroughWriter struct {
	web.ResponseWriter
}

// TestParamReadsServeMuxWildcards pins that path parameters reach Ctx.Param.
// The pattern syntax is ServeMux's own, which is the documented difference
// between this engine and a production one.
func TestParamReadsServeMuxWildcards(t *testing.T) {
	engine := New()

	engine.GET("/items/{id}", func(_ context.Context, c *web.Ctx) error {
		c.String(http.StatusOK, "%s", c.Param("id"))
		return nil
	})

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/items/42", nil))

	assert.Equal(t, http.StatusOK, recorder.Code, "已注册的通配路由应匹配")
	assert.Equal(t, "42", recorder.Body.String(), "Param 应返回 ServeMux 解析出的通配段")
}

// TestNewEngineMapsOptionsOntoTheHTTPServer pins the Options this factory does
// honour: the ones that become http.Server fields. They have no observable
// effect inside a test request -- dropping any one of them changes no response
// -- so nothing else in the suite would notice their loss, while a server
// built from this factory would quietly serve without the slow-request bound
// it was configured with. Each field gets a distinct value so a swapped pair
// fails rather than passes.
func TestNewEngineMapsOptionsOntoTheHTTPServer(t *testing.T) {
	built, err := Factory{}.NewEngine(web.Options{
		ReadTimeout:       11 * time.Second,
		ReadHeaderTimeout: 12 * time.Second,
		WriteTimeout:      13 * time.Second,
		IdleTimeout:       14 * time.Second,
		MaxHeaderBytes:    15000,
	})
	require.NoError(t, err, "NewEngine() 不应返回错误")
	engine, ok := built.(*Engine)
	require.True(t, ok, "NewEngine 应返回本包的 *Engine")

	assert.Equal(t, 11*time.Second, engine.srv.ReadTimeout, "ReadTimeout 必须落到 http.Server 上")
	assert.Equal(t, 12*time.Second, engine.srv.ReadHeaderTimeout, "ReadHeaderTimeout 必须落到 http.Server 上")
	assert.Equal(t, 13*time.Second, engine.srv.WriteTimeout, "WriteTimeout 必须落到 http.Server 上")
	assert.Equal(t, 14*time.Second, engine.srv.IdleTimeout, "IdleTimeout 必须落到 http.Server 上")
	assert.Equal(t, 15000, engine.srv.MaxHeaderBytes, "MaxHeaderBytes 必须落到 http.Server 上")
}

// TestUnmatchedRequestsReachNoRouteAndNoMethod pins the classification behind
// the catch-all pattern this engine registers. ServeMux would answer a
// method-mismatched request with its own 405 before any handler ran, which
// would silently bypass the NoMethod chain the Server installs; the catch-all
// suppresses that, so both chains must still receive the requests they own.
func TestUnmatchedRequestsReachNoRouteAndNoMethod(t *testing.T) {
	engine := New()
	engine.GET("/only-get", func(_ context.Context, c *web.Ctx) error {
		c.Status(http.StatusOK)
		return nil
	})
	engine.DELETE("/only-get", func(_ context.Context, c *web.Ctx) error {
		c.Status(http.StatusOK)
		return nil
	})
	engine.NoRoute([]web.Handler{func(_ context.Context, c *web.Ctx) error {
		c.String(http.StatusNotFound, "no-route")
		return nil
	}})
	engine.NoMethod([]web.Handler{func(_ context.Context, c *web.Ctx) error {
		c.String(http.StatusMethodNotAllowed, "no-method")
		return nil
	}})

	missing := httptest.NewRecorder()
	engine.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/absent", nil))
	assert.Equal(t, http.StatusNotFound, missing.Code, "未注册的路径应交给 NoRoute 链")
	assert.Equal(t, "no-route", missing.Body.String(), "NoRoute 链应实际运行")
	assert.Empty(t, missing.Result().Header.Get("Allow"), "404 不是方法不匹配，不得带 Allow")

	mismatched := httptest.NewRecorder()
	engine.ServeHTTP(mismatched, httptest.NewRequest(http.MethodPost, "/only-get", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, mismatched.Code,
		"仅注册了其他方法的路径应交给 NoMethod 链，而不是 ServeMux 自带的 405")
	assert.Equal(t, "no-method", mismatched.Body.String(), "NoMethod 链应实际运行")
	// 两个方法而非一个：单方法时「全部已注册方法」与「随便挑一个」无法区分。
	// HEAD 也在其中，因为 ServeMux 会用 GET 的处理器应答 HEAD——Allow 反映的是
	// 匹配器真正会接受的方法，而不是注册调用的清单。
	//
	// 判据是 Result().Header 而非 recorder.Header()：后者是活 map，事后写入也读得到，
	// 因此分不清「Allow 在状态行之前设置」与「之后设置」，而后者在真 socket 上
	// 永远到不了客户端。
	assert.Equal(t, "GET, HEAD, DELETE", mismatched.Result().Header.Get("Allow"),
		"405 必须按 web.Engine 端口的要求列出该路径其余全部已注册方法")
}

// TestShutdownForceClosesConnectionsThatRefuseToDrain pins the half of
// web.Engine's Shutdown contract that graceful draining cannot deliver on its
// own: once ctx is done, no established connection may still be serving. The
// handler here never returns within the test, so http.Server.Shutdown can only
// report its deadline; without the forced close that follows it, the client
// below would stay connected indefinitely and the deadline would mean nothing.
//
// The assertion is the client's own request failing, not elapsed time: a
// severed connection surfaces as a transport error on a request that was still
// in flight. The timeouts in this test are failure bounds only -- reaching one
// fails the test, and nothing passes because a duration elapsed.
func TestShutdownForceClosesConnectionsThatRefuseToDrain(t *testing.T) {
	engine, err := Factory{}.NewEngine(web.Options{})
	require.NoError(t, err, "NewEngine() 不应返回错误")

	handlerEntered := make(chan struct{})
	releaseHandler := make(chan struct{})
	// The handler outlives Shutdown on purpose; releasing it at test exit keeps
	// the goroutine from leaking into the rest of the package's run.
	defer close(releaseHandler)

	engine.Handle(http.MethodGet, "/block", []web.Handler{
		func(context.Context, *web.Ctx) error {
			close(handlerEntered)
			<-releaseHandler
			return nil
		},
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "监听回环端口失败")

	served := make(chan error, 1)
	go func() { served <- engine.Serve(listener) }()

	responded := make(chan error, 1)
	go func() {
		// The request deliberately carries no deadline of its own: the only
		// thing allowed to end it is the server severing the connection.
		response, requestErr := http.Get("http://" + listener.Addr().String() + "/block")
		if response != nil {
			_ = response.Body.Close()
		}
		responded <- requestErr
	}()

	select {
	case <-handlerEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("请求未能进入阻塞处理器，无法验证强制关闭")
	}

	// An already-expired deadline leaves draining no chance to succeed, which
	// is exactly the state the forced close exists for.
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	shutdownErr := engine.Shutdown(ctx)
	require.Error(t, shutdownErr, "排空未能在 deadline 内完成时，Shutdown 必须如实报告该失败")
	assert.ErrorIs(t, shutdownErr, context.DeadlineExceeded)

	select {
	case requestErr := <-responded:
		assert.Error(t, requestErr,
			"排空失败后 Shutdown 必须强制关闭残留连接，客户端请求不得仍然正常完成")
	case <-time.After(10 * time.Second):
		t.Fatal("排空失败后仍有已建立的连接在服务，Shutdown 没有强制关闭它")
	}

	select {
	case serveErr := <-served:
		assert.ErrorIs(t, serveErr, http.ErrServerClosed, "Serve 应以 ErrServerClosed 结束")
	case <-time.After(10 * time.Second):
		t.Fatal("Serve 未能返回，监听器没有被关闭")
	}
}
