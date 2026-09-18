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

	assert.Equal(t, http.StatusNoContent, recorder.Code, "the status the route handler set must reach the response")
	assert.Equal(t, []string{"global-before", "route", "global-after"}, order,
		"the global chain must be entered ahead of the route chain and resumed once the route chain returns")
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

	assert.False(t, downstreamRan, "no handler after the aborting one may run")
	assert.Equal(t, http.StatusForbidden, recorder.Code, "the status the aborting handler wrote must survive")
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
		assert.False(t, c.Writer().Written(), "Status must only record the code -- it must not commit the response")
		assert.Equal(t, http.StatusTeapot, c.Writer().Status(), "the status Status recorded must be readable back")
		assert.Equal(t, -1, c.Writer().Size(), "Size must be -1 while no body has been written yet")
		return nil
	})

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/recorded", nil))

	assert.Equal(t, http.StatusTeapot, recorder.Code,
		"the recorded status must be committed when the chain ends -- otherwise a handler that only calls Status produces no response at all")
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
		"the wrapper must read back the status the handler recorded, not the default one")
	assert.Equal(t, http.StatusAccepted, recorder.Code,
		"the recorded status must still be committed once the original writer has been restored")
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

	assert.Equal(t, http.StatusOK, recorder.Code, "the registered wildcard route must match")
	assert.Equal(t, "42", recorder.Body.String(), "Param must return the wildcard segment ServeMux parsed")
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
	require.NoError(t, err, "NewEngine() must not fail")
	engine, ok := built.(*Engine)
	require.True(t, ok, "NewEngine must return this package's *Engine")

	assert.Equal(t, 11*time.Second, engine.srv.ReadTimeout, "ReadTimeout must reach the http.Server")
	assert.Equal(t, 12*time.Second, engine.srv.ReadHeaderTimeout, "ReadHeaderTimeout must reach the http.Server")
	assert.Equal(t, 13*time.Second, engine.srv.WriteTimeout, "WriteTimeout must reach the http.Server")
	assert.Equal(t, 14*time.Second, engine.srv.IdleTimeout, "IdleTimeout must reach the http.Server")
	assert.Equal(t, 15000, engine.srv.MaxHeaderBytes, "MaxHeaderBytes must reach the http.Server")
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
	assert.Equal(t, http.StatusNotFound, missing.Code, "an unregistered path must be handed to the NoRoute chain")
	assert.Equal(t, "no-route", missing.Body.String(), "The NoRoute chain must actually run")
	assert.Empty(t, missing.Result().Header.Get("Allow"), "a 404 is not a method mismatch, so it must carry no Allow header")

	mismatched := httptest.NewRecorder()
	engine.ServeHTTP(mismatched, httptest.NewRequest(http.MethodPost, "/only-get", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, mismatched.Code,
		"a path registered only under other methods must be handed to the NoMethod chain, not answered by ServeMux's own 405")
	assert.Equal(t, "no-method", mismatched.Body.String(), "The NoMethod chain must actually run")
	// Two methods rather than one: with a single method, "every registered
	// method" and "any one of them" are indistinguishable. HEAD is among them
	// because ServeMux answers HEAD with the GET handler -- Allow reflects the
	// methods the matcher will really accept, not the list of registration
	// calls.
	//
	// The assertion reads Result().Header rather than recorder.Header(): the
	// latter is a live map that also shows a write made afterwards, so it cannot
	// tell "Allow set before the status line" from "set after it", and the
	// latter never reaches the client over a real socket.
	assert.Equal(t, "GET, HEAD, DELETE", mismatched.Result().Header.Get("Allow"),
		"a 405 must list every other method registered for the path, as the web.Engine port requires")
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
	require.NoError(t, err, "NewEngine() must not fail")

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
		t.Fatal("the request never entered the blocking handler, so the forced close cannot be observed")
	}

	// An already-expired deadline leaves draining no chance to succeed, which
	// is exactly the state the forced close exists for.
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	shutdownErr := engine.Shutdown(ctx)
	require.Error(t, shutdownErr, "Shutdown must report the failure honestly when draining could not finish within the deadline")
	assert.ErrorIs(t, shutdownErr, context.DeadlineExceeded)

	select {
	case requestErr := <-responded:
		assert.Error(t, requestErr,
			"Once draining has failed, Shutdown must force the remaining connection closed -- the client's request must not still complete normally")
	case <-time.After(10 * time.Second):
		t.Fatal("an established connection was still being served after draining failed: Shutdown did not force it closed")
	}

	select {
	case serveErr := <-served:
		assert.ErrorIs(t, serveErr, http.ErrServerClosed, "Serve must end with ErrServerClosed")
	case <-time.After(10 * time.Second):
		t.Fatal("Serve never returned: the listener was not closed")
	}
}

// bufferingWriter is the wrapper a gzip-, timeout-, or envelope-style
// middleware installs over the request's writer: it holds the status and the
// body until the middleware unwinds, then replays both onto the writer it
// replaced.
//
// The replay is what gives the test below its discriminating power. A wrapper
// that only records what passes through it produces the same observable
// response whether or not something committed the real writer behind its back,
// so it would pass against precisely the bug this test exists to catch.
type bufferingWriter struct {
	wrapped web.ResponseWriter
	status  int
	body    []byte
	written bool
}

func newBufferingWriter(wrapped web.ResponseWriter) *bufferingWriter {
	return &bufferingWriter{wrapped: wrapped, status: http.StatusOK}
}

func (w *bufferingWriter) Header() http.Header { return w.wrapped.Header() }

func (w *bufferingWriter) WriteHeader(code int) {
	if code <= 0 || w.written {
		return
	}
	w.status = code
	w.written = true
}

func (w *bufferingWriter) Write(data []byte) (int, error) {
	w.WriteHeader(w.status)
	w.body = append(w.body, data...)
	return len(data), nil
}

func (w *bufferingWriter) Flush() {}

func (w *bufferingWriter) Status() int { return w.status }

func (w *bufferingWriter) Size() int {
	if !w.written {
		return noWritten
	}
	return len(w.body)
}

func (w *bufferingWriter) Written() bool { return w.written }

// replay commits the buffered response onto the writer this wrapper replaced,
// which is what every real buffering middleware does as it unwinds.
func (w *bufferingWriter) replay() {
	w.wrapped.WriteHeader(w.status)
	_, _ = w.wrapped.Write(w.body)
}

// bufferResponse is the middleware form of bufferingWriter.
func bufferResponse(_ context.Context, c *web.Ctx) error {
	previous := c.Writer()
	buffered := newBufferingWriter(previous)
	c.SetWriter(buffered)
	c.Next()
	c.SetWriter(previous)
	buffered.replay()
	return nil
}

// TestBodylessStatusCommitsThroughTheInstalledWriter pins that a renderer never
// commits past a wrapper a middleware installed. A status that forbids a body
// (204, 304, 1xx) is the only case where a renderer has nothing left to write
// and is therefore tempted to commit the response itself; committing through
// the bottom of the writer chain at that moment sends the wrapper's buffered
// status nowhere, because the real writer is already committed by the time the
// wrapper replays.
//
// JSON and String are asserted to agree. They are two renderings of the same
// neutral decision -- "this status carries no body" -- so a regression on
// either side breaks the equality, and the port's whole purpose is that a
// middleware behaves identically whichever renderer a handler reached for.
func TestBodylessStatusCommitsThroughTheInstalledWriter(t *testing.T) {
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
		engine := New()
		engine.Use(bufferResponse)
		engine.GET("/bodyless", renderer.render)

		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/bodyless", nil))

		// Result() is the response as it was committed, not the recorder's live
		// header map, so a status written after the wrapper replayed cannot be
		// mistaken for one the client received.
		result := recorder.Result()
		require.NoError(t, result.Body.Close())
		committed[renderer.name] = result.StatusCode

		assert.Equal(t, http.StatusNoContent, result.StatusCode,
			"%s must commit a bodyless status through the writer currently installed, so the status the wrapper replays is the one that reaches the client", renderer.name)
		assert.Empty(t, recorder.Body.String(), "%s must not write a response body for a bodyless status", renderer.name)
	}

	assert.Equal(t, committed["String"], committed["JSON"],
		"JSON and String must commit at the same moment -- a regression on either side must surface here")
}
