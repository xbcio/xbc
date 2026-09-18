package gin

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
)

const (
	// writeDeadlineBudget is the server-wide web.write_timeout these tests run
	// under, and streamChunks x streamInterval deliberately exceeds it: a
	// response that finished inside the budget would prove nothing either way.
	writeDeadlineBudget = 200 * time.Millisecond
	streamInterval      = 150 * time.Millisecond
	streamChunks        = 4
)

// TestAStreamingHandlerOutlivesTheWriteTimeoutByClearingItsOwnDeadline is the
// reachability guarantee behind every long-lived response the framework can
// serve: server-sent events, a progress stream, a chunked export.
//
// web.write_timeout is one deadline for the whole response, so any response
// that outlives it has to lift it for itself, and http.ResponseController is the
// only way to do that per connection.
//
// This is the default path, with nothing between the handler and gin's own
// writer; the wrapped case a deployment with buffering middleware has is the
// test below.
//
// Both halves are asserted, because either one alone is satisfied by a broken
// stack: a handler that received no deadline at all would pass this test, and
// the control below is what rules that out.
func TestAStreamingHandlerOutlivesTheWriteTimeoutByClearingItsOwnDeadline(t *testing.T) {
	body, err := streamThroughEngine(t, func(c *web.Ctx) error {
		// The zero time removes the deadline rather than extending it. A
		// handler that streams for an unknown length cannot pick a later one,
		// which is the reason a server-wide budget cannot express this case.
		return http.NewResponseController(c.Writer()).SetWriteDeadline(time.Time{})
	})

	require.NoError(t, err, "a handler that cleared its write deadline must be able to outlive web.write_timeout")
	assert.Equal(t, "0123", body, "every chunk written after the budget elapsed must still reach the client")
}

// TestAStreamingHandlerThatKeepsTheWriteTimeoutIsCutOff is the control, and it
// is what gives the test above its meaning: it shows the deadline was real and
// was being enforced on exactly this path.
//
// It also documents the default a deployment gets. web.write_timeout applies to
// the whole response, so a handler that streams past it is severed mid-body --
// not refused, not answered with a status, just cut. That is the correct default
// for ordinary request/response traffic, and it is why a streaming route has to
// opt out explicitly rather than being handed an unbounded write by default.
func TestAStreamingHandlerThatKeepsTheWriteTimeoutIsCutOff(t *testing.T) {
	body, err := streamThroughEngine(t, func(*web.Ctx) error { return nil })

	require.Error(t, err, "web.write_timeout must still sever a response that outlives it")
	assert.NotEqual(t, "0123", body, "the whole stream cannot have arrived under an enforced deadline")
}

// TestAStreamingHandlerReachesTheConnectionThroughAWrappedWriter is the same
// guarantee one link deeper, where it can actually break.
//
// A deployment that selected gzip, request timeout or idempotency has a
// buffering wrapper between the handler and the socket, and the engine holds
// that wrapper behind its own gin adapter. http.ResponseController reaches the
// connection only if every link forwards, so this drives the whole descent: the
// adapter's shim, then the wrapper, then gin's writer. Breaking Unwrap on any of
// them turns lifting the deadline into an error the handler cannot work around.
func TestAStreamingHandlerReachesTheConnectionThroughAWrappedWriter(t *testing.T) {
	body, err := streamThroughEngine(t, func(c *web.Ctx) error {
		c.SetWriter(&passthroughWriter{ResponseWriter: c.Writer()})
		return http.NewResponseController(c.Writer()).SetWriteDeadline(time.Time{})
	})

	require.NoError(t, err, "a wrapped writer must not hide the connection from http.ResponseController")
	assert.Equal(t, "0123", body)
}

// streamThroughEngine serves one chunked response that takes longer to write
// than writeDeadlineBudget allows, and returns what the client managed to read.
//
// prepare runs inside the handler before the first byte, which is where a
// streaming route decides what to do about the deadline. Everything else is held
// identical between the two tests, so the deadline is the only difference
// between them.
func streamThroughEngine(t *testing.T, prepare func(*web.Ctx) error) (string, error) {
	t.Helper()
	restoreProcessGlobals(t)

	engine, err := Factory{}.NewEngine(web.Options{WriteTimeout: writeDeadlineBudget})
	require.NoError(t, err)

	handlerFailed := make(chan error, 1)
	engine.Handle(http.MethodGet, "/stream", []web.Handler{
		func(_ context.Context, c *web.Ctx) error {
			if prepareErr := prepare(c); prepareErr != nil {
				handlerFailed <- prepareErr
				return prepareErr
			}
			writer := c.Writer()
			writer.Header().Set("Content-Type", "text/event-stream")
			for chunk := 0; chunk < streamChunks; chunk++ {
				// A write error is expected in the control case, so it is not
				// reported: the client's truncated read is the assertion there.
				// Writing and flushing each chunk separately is what makes the
				// response chunked and therefore observably severed.
				_, _ = fmt.Fprintf(writer, "%d", chunk)
				writer.Flush()
				time.Sleep(streamInterval)
			}
			return nil
		},
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	served := make(chan error, 1)
	go func() { served <- engine.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, engine.Shutdown(ctx))
		require.ErrorIs(t, <-served, http.ErrServerClosed)
		select {
		case prepareErr := <-handlerFailed:
			t.Fatalf("preparing the streaming response failed: %v", prepareErr)
		default:
		}
	})

	// The client carries no timeout of its own: the only thing allowed to end
	// this response is the server's own deadline, so a client-side deadline
	// would make the two cases indistinguishable.
	response, err := http.Get("http://" + listener.Addr().String() + "/stream")
	require.NoError(t, err, "the response headers arrive well inside the budget in both cases")
	defer func() { _ = response.Body.Close() }()

	body, readErr := io.ReadAll(response.Body)
	return string(body), readErr
}
