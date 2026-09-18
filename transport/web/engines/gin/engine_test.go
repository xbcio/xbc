package gin

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
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
	// NewEngine now sets gin's mode itself via applyProcessGlobals, so this
	// only restores whatever this test's own NewEngine call leaves behind.
	restoreProcessGlobals(t)

	engine, err := Factory{}.NewEngine(web.Options{})
	require.NoError(t, err, "NewEngine() must not return an error")

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
	require.NoError(t, err, "failed to listen on a loopback port")

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
		t.Fatal("the request never entered the blocking handler, so the forced close cannot be verified")
	}

	// An already-expired deadline leaves draining no chance to succeed, which
	// is exactly the state the forced close exists for.
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	shutdownErr := engine.Shutdown(ctx)
	require.Error(t, shutdownErr, "Shutdown must report the failure faithfully when draining could not finish within the deadline")
	assert.ErrorIs(t, shutdownErr, context.DeadlineExceeded)

	select {
	case requestErr := <-responded:
		assert.Error(t, requestErr,
			"Shutdown must force the lingering connection closed once draining failed -- the client's request must not still complete normally")
	case <-time.After(10 * time.Second):
		t.Fatal("an established connection is still being served after draining failed: Shutdown did not force it closed")
	}

	select {
	case serveErr := <-served:
		assert.ErrorIs(t, serveErr, http.ErrServerClosed, "Serve must end with ErrServerClosed")
	case <-time.After(10 * time.Second):
		t.Fatal("Serve never returned, so the listener was not closed")
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
	// NewEngine now sets gin's mode itself via applyProcessGlobals, so an
	// explicit SetMode(TestMode) here would only be overwritten; the
	// restoration below keeps that mode choice from leaking into other tests.
	restoreProcessGlobals(t)

	built, err := Factory{}.NewEngine(web.Options{
		ReadTimeout:       11 * time.Second,
		ReadHeaderTimeout: 12 * time.Second,
		WriteTimeout:      13 * time.Second,
		IdleTimeout:       14 * time.Second,
		MaxHeaderBytes:    15000,
	})
	require.NoError(t, err, "NewEngine() must not return an error")
	adapter, ok := built.(*engine)
	require.True(t, ok, "NewEngine must return this package's *engine")

	assert.Equal(t, 11*time.Second, adapter.srv.ReadTimeout, "ReadTimeout must land on the http.Server")
	assert.Equal(t, 12*time.Second, adapter.srv.ReadHeaderTimeout, "ReadHeaderTimeout must land on the http.Server")
	assert.Equal(t, 13*time.Second, adapter.srv.WriteTimeout, "WriteTimeout must land on the http.Server")
	assert.Equal(t, 14*time.Second, adapter.srv.IdleTimeout, "IdleTimeout must land on the http.Server")
	assert.Equal(t, 15000, adapter.srv.MaxHeaderBytes, "MaxHeaderBytes must land on the http.Server")
}

// TestNewEngineAppliesProcessGlobalsBeforeGinNew pins the ordering documented
// at the applyProcessGlobals call site in NewEngine: gin decides at
// construction time whether to print its debug banner, so the mode and
// DefaultWriter must already be switched to the logger before ginlib.New()
// runs, not after. A version that called ginlib.New() first would still
// compile and pass every other test in this package -- New()'s
// construction-time banner is the only place where being one line too late
// becomes observable, because everything else this adapter does happens
// after construction.
func TestNewEngineAppliesProcessGlobalsBeforeGinNew(t *testing.T) {
	restoreProcessGlobals(t)
	logger := &capturingLogger{Logger: log.Nop(), debug: true}

	_, err := Factory{}.NewEngine(web.Options{Logger: logger})
	require.NoError(t, err, "NewEngine() must not return an error")

	require.Len(t, logger.info, 1,
		"applyProcessGlobals must run before ginlib.New() so that the debug banner New() prints at construction time lands on the logger rather than the process's default output")
	assert.Contains(t, logger.info[0], `Running in "debug" mode`,
		"what was captured must be gin's construction-time debug banner, not just any output")
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
	restoreProcessGlobals(t)

	built, err := Factory{}.NewEngine(web.Options{})
	require.NoError(t, err, "NewEngine() must not return an error")
	adapter, ok := built.(*engine)
	require.True(t, ok, "NewEngine must return this package's *engine")

	ok200 := func(_ context.Context, c *web.Ctx) error { c.Status(http.StatusOK); return nil }
	adapter.Handle(http.MethodGet, "/only-get", []web.Handler{ok200})
	adapter.Handle(http.MethodDelete, "/only-get", []web.Handler{ok200})
	adapter.NoMethod([]web.Handler{func(_ context.Context, c *web.Ctx) error {
		c.Status(http.StatusMethodNotAllowed)
		return nil
	}})

	recorder := httptest.NewRecorder()
	adapter.e.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/only-get", nil))

	require.Equal(t, http.StatusMethodNotAllowed, recorder.Code, "a method mismatch must be handed to the NoMethod chain")
	// Comparing verbatim would tie "every method is listed" together with "the
	// order happens to be exactly this one", while the port only asks for the
	// former.
	// The judgement is taken from Result().Header rather than recorder.Header():
	// the latter is a live map that reads back writes made after the fact, so an
	// Allow set after the status line -- which on a real socket is no Allow at
	// all -- would still fool the assertion, and silent breakage after a
	// dependency upgrade, the very thing this guard exists to catch, takes
	// precisely that shape.
	assert.ElementsMatch(t, []string{"GET", "DELETE"},
		strings.Split(recorder.Result().Header.Get("Allow"), ", "),
		"the 405 must list every other method already registered for that path")
}

// replayingWriter is the wrapper a gzip-, timeout-, or envelope-style
// middleware installs over the request's writer: it holds the status and the
// body until the middleware unwinds, then replays both onto the writer it
// replaced.
//
// The replay is what gives the test below its discriminating power. A wrapper
// that only records what passes through it produces the same observable
// response whether or not something committed the real writer behind its back,
// which is exactly the asymmetry this test pins against the neutral engine.
type replayingWriter struct {
	wrapped web.ResponseWriter
	status  int
	body    []byte
	written bool
}

func newReplayingWriter(wrapped web.ResponseWriter) *replayingWriter {
	return &replayingWriter{wrapped: wrapped, status: http.StatusOK}
}

func (w *replayingWriter) Header() http.Header { return w.wrapped.Header() }

func (w *replayingWriter) WriteHeader(code int) {
	if code <= 0 || w.written {
		return
	}
	w.status = code
	w.written = true
}

func (w *replayingWriter) Write(data []byte) (int, error) {
	w.WriteHeader(w.status)
	w.body = append(w.body, data...)
	return len(data), nil
}

func (w *replayingWriter) Flush() {}

func (w *replayingWriter) Status() int { return w.status }

// Size answers gin's documented -1 sentinel until the first write.
func (w *replayingWriter) Size() int {
	if !w.written {
		return -1
	}
	return len(w.body)
}

func (w *replayingWriter) Written() bool { return w.written }

// replay commits the buffered response onto the writer this wrapper replaced,
// which is what every real buffering middleware does as it unwinds.
func (w *replayingWriter) replay() {
	w.wrapped.WriteHeader(w.status)
	_, _ = w.wrapped.Write(w.body)
}

// bufferResponse is the middleware form of replayingWriter.
func bufferResponse(_ context.Context, c *web.Ctx) error {
	previous := c.Writer()
	buffered := newReplayingWriter(previous)
	c.SetWriter(buffered)
	c.Next()
	c.SetWriter(previous)
	buffered.replay()
	return nil
}

// TestBodylessStatusCommitsThroughTheInstalledWriter is the gin-side twin of
// the neutral engine's test of the same name. A status that forbids a body
// (204, 304, 1xx) is the only case where a renderer has nothing left to write
// and is therefore tempted to commit the response itself; committing past a
// wrapper a middleware installed sends that wrapper's buffered status nowhere.
//
// Both engines are pinned because this is precisely the kind of divergence the
// Engine port exists to rule out: the same neutral middleware must produce the
// same response whichever adapter is underneath it, and JSON must agree with
// String on either one.
func TestBodylessStatusCommitsThroughTheInstalledWriter(t *testing.T) {
	restoreProcessGlobals(t)

	renderers := []struct {
		name   string
		render web.Handler
	}{
		{name: "JSON", render: func(_ context.Context, c *web.Ctx) error {
			c.JSON(http.StatusNoContent, map[string]string{"dropped": "body"})
			return nil
		}},
		{name: "String", render: func(_ context.Context, c *web.Ctx) error {
			c.String(http.StatusNoContent, "dropped")
			return nil
		}},
	}

	committed := make(map[string]int, len(renderers))
	for _, renderer := range renderers {
		built, err := Factory{}.NewEngine(web.Options{})
		require.NoError(t, err, "NewEngine() must not return an error")
		adapter, ok := built.(*engine)
		require.True(t, ok, "NewEngine must return this package's *engine")
		adapter.Handle(http.MethodGet, "/bodyless", []web.Handler{bufferResponse, renderer.render})

		recorder := httptest.NewRecorder()
		adapter.e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/bodyless", nil))

		// Result() is the response as it was committed, not the recorder's live
		// header map, so a status written after the wrapper replayed cannot be
		// mistaken for one the client received.
		result := recorder.Result()
		require.NoError(t, result.Body.Close())
		committed[renderer.name] = result.StatusCode

		assert.Equal(t, http.StatusNoContent, result.StatusCode,
			"%s must commit through the writer currently installed on a bodyless status, so the status the wrapper replays reaches the client", renderer.name)
		assert.Empty(t, recorder.Body.String(), "%s must not write a body for a bodyless status", renderer.name)
	}

	assert.Equal(t, committed["String"], committed["JSON"],
		"JSON and String must commit at the same point -- a regression on either side must surface here")
}
