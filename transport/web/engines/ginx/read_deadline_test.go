package ginx

import (
	"bufio"
	"context"
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
	// readDeadlineBudget is the read deadline a handler sets for itself. The
	// stalled client never finishes its body, so the deadline is the only thing
	// that can end the read; the keep-alive test waits longer than it to prove
	// the deadline is no longer in force.
	readDeadlineBudget = 150 * time.Millisecond
	readDeadlineWait   = 400 * time.Millisecond
)

// TestAHandlerReadDeadlineReachesTheConnectionAndTruncatesAStalledBody is the
// read-side twin of the write deadline guarantee, and it covers the descent
// that can actually break: the handler holds only the neutral writer, and
// http.ResponseController must follow Unwrap from there to the live connection.
//
// A client that announces 100 body bytes and sends 3 can only be released by
// the deadline the handler set, so a response arriving at all proves
// SetReadDeadline reached the socket; the 408 proves the read error that came
// back is the one the error boundary recognizes as the client's stall.
func TestAHandlerReadDeadlineReachesTheConnectionAndTruncatesAStalledBody(t *testing.T) {
	addr := serveReadDeadlineEngine(t, func(c *web.Ctx) {})

	response := stalledBodyRequest(t, addr)

	assert.Equal(t, http.StatusRequestTimeout, response.StatusCode)
	assert.Contains(t, readBody(t, response), `"request_timeout"`)
}

// TestAHandlerReadDeadlineReachesTheConnectionThroughAWrappedWriter is the same
// guarantee one link deeper. A deployment that selected gzip, request timeout or
// idempotency has a buffering wrapper between the handler and the socket, held
// behind the adapter's own shim. Breaking Unwrap on any link turns setting the
// deadline into an error the handler cannot work around.
func TestAHandlerReadDeadlineReachesTheConnectionThroughAWrappedWriter(t *testing.T) {
	addr := serveReadDeadlineEngine(t, func(c *web.Ctx) {
		c.SetWriter(&passthroughWriter{ResponseWriter: c.Writer()})
	})

	response := stalledBodyRequest(t, addr)

	assert.Equal(t, http.StatusRequestTimeout, response.StatusCode,
		"a wrapped writer must not hide the connection from http.ResponseController")
	assert.Contains(t, readBody(t, response), `"request_timeout"`)
}

// TestAHandlerReadDeadlineDoesNotOutliveItsRequestOnAKeepAliveConnection pins
// the other side of a per-request deadline. The first request sets a short read
// deadline and finishes at once; the second arrives on the same connection after
// that moment has passed. If the deadline were still armed, reading the second
// request would fail on the connection and no response would come back.
func TestAHandlerReadDeadlineDoesNotOutliveItsRequestOnAKeepAliveConnection(t *testing.T) {
	addr := serveReadDeadlineEngine(t, func(c *web.Ctx) {})

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))
	reader := bufio.NewReader(conn)

	for attempt, wait := range []time.Duration{0, readDeadlineWait} {
		time.Sleep(wait)
		_, err = io.WriteString(conn, "GET /armed HTTP/1.1\r\nHost: example.test\r\n\r\n")
		require.NoError(t, err, "request %d must be accepted on the same connection", attempt+1)

		response, readErr := http.ReadResponse(reader, nil)
		require.NoError(t, readErr, "request %d must be answered: a deadline that leaked would have severed the connection", attempt+1)
		assert.Equal(t, http.StatusOK, response.StatusCode)
		assert.Equal(t, "armed", readBody(t, response))
	}
}

// serveReadDeadlineEngine serves two routes on a real gin engine and a real
// socket. prepare runs inside each handler before the deadline is set, which is
// where a test swaps in a wrapped writer. Everything else is held identical
// across the tests, so the writer chain is the only difference between them.
//
// /stall sets a read deadline and returns the read error untouched, the way a
// handler that does not route it through ParamError does. /armed sets the same
// deadline and finishes immediately.
func serveReadDeadlineEngine(t *testing.T, prepare func(*web.Ctx)) string {
	t.Helper()
	restoreProcessGlobals(t)

	engine, err := Factory{}.NewEngine(web.Options{})
	require.NoError(t, err)

	setDeadline := func(c *web.Ctx) error {
		prepare(c)
		return http.NewResponseController(c.Writer()).SetReadDeadline(time.Now().Add(readDeadlineBudget))
	}
	engine.Handle(http.MethodPost, "/stall", []web.Handler{
		func(_ context.Context, c *web.Ctx) error {
			if err := setDeadline(c); err != nil {
				return err
			}
			_, err := io.ReadAll(c.Request().Body)
			return err
		},
	})
	engine.Handle(http.MethodGet, "/armed", []web.Handler{
		func(_ context.Context, c *web.Ctx) error {
			if err := setDeadline(c); err != nil {
				return err
			}
			c.String(http.StatusOK, "armed")
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
	})
	return listener.Addr().String()
}

// stalledBodyRequest announces 100 body bytes, sends 3, and waits for the
// server's answer. The raw connection is what makes the stall real: an HTTP
// client library would either send the whole body or fail before the server
// could respond.
func stalledBodyRequest(t *testing.T, addr string) *http.Response {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = io.WriteString(conn, "POST /stall HTTP/1.1\r\nHost: example.test\r\nContent-Type: text/plain\r\nContent-Length: 100\r\n\r\nabc")
	require.NoError(t, err)

	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err, "the server must answer instead of leaving the stalled client hanging")
	return response
}

func readBody(t *testing.T, response *http.Response) string {
	t.Helper()
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return string(body)
}
