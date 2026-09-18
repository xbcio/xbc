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

	assert.False(t, rc.Writer().Written(), "Status must only record the status, never commit the response")
	assert.Equal(t, http.StatusTeapot, rc.Writer().Status(), "Status must have recorded the status")
	assert.Equal(t, http.StatusOK, recorder.Code, "the recorder beneath must not have received a committed status line")
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

	require.False(t, base.Written(), "installing a writer must not commit anything")
	require.NoError(t, rc.JSON(http.StatusCreated, map[string]string{"name": "xbc"}))

	assert.True(t, base.Written(), "gin's rendering must land on the neutral writer SetWriter installed")
	assert.Equal(t, http.StatusCreated, base.Status(), "the status must reach the neutral writer through the shim")
	assert.JSONEq(t, `{"name":"xbc"}`, base.recorder.Body.String(), "the body must be written into the neutral writer")
	assert.Equal(t, http.StatusOK, recorder.Code, "the writer that was replaced must receive nothing further")

	assert.Same(t, rc.Writer(), rc.c.Writer, "Writer must return the writer gin currently holds")
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

	assert.False(t, wrapper.Written(), "Status must still only record, never commit the response")
	assert.Equal(t, http.StatusAccepted, wrapper.Status(),
		"the status the wrapper reads must be the one the handler recorded, not the default")

	rc.SetWriter(original)
	rc.c.Writer.WriteHeaderNow()

	assert.Equal(t, http.StatusAccepted, recorder.Code, "the recorded status must still be committed after the original writer is restored")
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

	require.Error(t, err, "a render failure must be reported to the caller")
	var unsupported *json.UnsupportedTypeError
	assert.ErrorAs(t, err, &unsupported, "the engine's render error must be reported as it is")
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
		"a successful render must not be reported as a failure because the chain already carried an error")
	assert.JSONEq(t, `{"name":"xbc"}`, recorder.Body.String(), "the body must be written out as usual")
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
	require.NoError(t, rc.Bind(&payload), "a form-encoded body must bind: Bind has to delegate to the engine's own content negotiation")
	assert.Equal(t, "xbc", payload.Name, "Bind must pick the form binder based on the Content-Type")
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

	require.Error(t, err, "a body whose types do not match must fail to bind")
	_, native := err.(*json.UnmarshalTypeError)
	assert.True(t, native, "Bind must return the engine's binding error as it is -- wrapping it in ParamError is web's job")
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

	require.NoError(t, bindErr, "BindURI must not return an error")
	assert.Equal(t, "42", param, "Param must read the route's wildcard segment")
	assert.Equal(t, "42", bound.ID, "BindURI must populate the uri-tagged field")
}

// TestClientIPForwardsToTheEngine pins the doc comment's claim that ClientIP
// is the engine's answer. With no trusted proxy the forwarded header must be
// ignored, which is a decision SetTrustedProxies made inside gin; anything
// derived here from the request alone would have to re-decide it and could
// disagree.
func TestClientIPForwardsToTheEngine(t *testing.T) {
	ginlib.SetMode(ginlib.TestMode)
	engine := ginlib.New()
	require.NoError(t, engine.SetTrustedProxies(nil), "SetTrustedProxies(nil) must not fail")

	var got string
	engine.GET("/ip", func(c *ginlib.Context) { got = newRequestContext(c).ClientIP() })

	request := httptest.NewRequest(http.MethodGet, "/ip", nil)
	request.RemoteAddr = "203.0.113.7:1234"
	request.Header.Set("X-Forwarded-For", "198.51.100.9")
	engine.ServeHTTP(httptest.NewRecorder(), request)

	assert.Equal(t, "203.0.113.7", got,
		"with no trusted proxy ClientIP must give the engine's own answer and ignore X-Forwarded-For")
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
		"a request from a trusted proxy must take X-Forwarded-For at its word -- an answer only a real forward to the engine can produce")
	assert.Equal(t, "203.0.113.7", call("203.0.113.7:51234"),
		"a request from an untrusted proxy must ignore X-Forwarded-For, in agreement with the engine's own judgement")
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

	assert.Equal(t, []string{"first"}, ran, "no handler after Abort may still run")
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
		"Next must not return until the rest of the chain has run")
}

// TestSetRequestIsVisibleThroughRequest pins the pairing the rest of the
// framework depends on: SetContext publishes a context by rewriting the
// request, and every downstream reader finds it through Request.
func TestSetRequestIsVisibleThroughRequest(t *testing.T) {
	recorder := httptest.NewRecorder()
	rc := newTestRequestContext(t, recorder)

	replacement := httptest.NewRequest(http.MethodPut, "/replaced", nil)
	rc.SetRequest(replacement)

	assert.Same(t, replacement, rc.Request(), "Request must return the request SetRequest installed")
	assert.Same(t, replacement, rc.c.Request, "SetRequest must rewrite the request the engine holds")
}
