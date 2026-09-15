package gin

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ginlib "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
)

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
	ginlib.SetMode(ginlib.TestMode)

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

// TestNewEngineMapsOptionsOntoTheHTTPServer pins the half of NewEngine that
// has no observable effect inside a test request: the neutral Options that
// become http.Server fields. Those timeouts and the header-size bound are a
// server's only defence against a slow-request attack -- Slowloris and its
// relatives -- and dropping any one of them changes no response at all, so
// every other test in this repository would stay green while a deployed server
// lost its bound. Each field therefore gets a distinct value, which is also
// what makes a swapped pair fail rather than pass.
func TestNewEngineMapsOptionsOntoTheHTTPServer(t *testing.T) {
	ginlib.SetMode(ginlib.TestMode)

	built, err := Factory{}.NewEngine(web.Options{
		ReadTimeout:       11 * time.Second,
		ReadHeaderTimeout: 12 * time.Second,
		WriteTimeout:      13 * time.Second,
		IdleTimeout:       14 * time.Second,
		MaxHeaderBytes:    15000,
	})
	require.NoError(t, err, "NewEngine() 不应返回错误")
	adapter, ok := built.(*engine)
	require.True(t, ok, "NewEngine 应返回本包的 *engine")

	assert.Equal(t, 11*time.Second, adapter.srv.ReadTimeout, "ReadTimeout 必须落到 http.Server 上")
	assert.Equal(t, 12*time.Second, adapter.srv.ReadHeaderTimeout, "ReadHeaderTimeout 必须落到 http.Server 上")
	assert.Equal(t, 13*time.Second, adapter.srv.WriteTimeout, "WriteTimeout 必须落到 http.Server 上")
	assert.Equal(t, 14*time.Second, adapter.srv.IdleTimeout, "IdleTimeout 必须落到 http.Server 上")
	assert.Equal(t, 15000, adapter.srv.MaxHeaderBytes, "MaxHeaderBytes 必须落到 http.Server 上")
}

// TestMethodNotAllowedSetsAllowHeader pins this adapter's half of the
// web.Engine NoMethod contract. The framework's 405 Problem Detail is produced
// by the NoMethod chain, which knows only that the method was wrong -- the list
// of methods the path does support exists solely inside the engine's matcher,
// so an adapter that forwards the chain without the header would emit a 405
// that reads correctly and still violates RFC 9110 §15.5.6.
//
// Gin sets Allow itself before dispatching to NoMethod. That is precisely why
// this test is here: behaviour inherited from a dependency is the kind that
// disappears silently on an upgrade.
func TestMethodNotAllowedSetsAllowHeader(t *testing.T) {
	ginlib.SetMode(ginlib.TestMode)

	built, err := Factory{}.NewEngine(web.Options{HandleMethodNotAllowed: true})
	require.NoError(t, err, "NewEngine() 不应返回错误")
	adapter, ok := built.(*engine)
	require.True(t, ok, "NewEngine 应返回本包的 *engine")

	ok200 := func(_ context.Context, c *web.Ctx) error { c.Status(http.StatusOK); return nil }
	adapter.Handle(http.MethodGet, "/only-get", []web.Handler{ok200})
	adapter.Handle(http.MethodDelete, "/only-get", []web.Handler{ok200})
	adapter.NoMethod([]web.Handler{func(_ context.Context, c *web.Ctx) error {
		c.Status(http.StatusMethodNotAllowed)
		return nil
	}})

	recorder := httptest.NewRecorder()
	adapter.e.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/only-get", nil))

	require.Equal(t, http.StatusMethodNotAllowed, recorder.Code, "方法不匹配应交给 NoMethod 链")
	// 逐字比较会把「列全了」和「顺序恰好如此」绑在一起，而端口只要求列全。
	// 判据取 Result().Header 而非 recorder.Header()：后者是活 map，事后写入也读得到，
	// 于是「Allow 设在状态行之后」——在真 socket 上等于没有 Allow——照样能骗过断言，
	// 这条门禁本来要防的「依赖升级后静默失效」恰好就是这种形态。
	assert.ElementsMatch(t, []string{"GET", "DELETE"},
		strings.Split(recorder.Result().Header.Get("Allow"), ", "),
		"405 必须列出该路径其余全部已注册方法")
}
