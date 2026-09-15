package gin

import (
	"context"
	"net"
	"net/http"
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
