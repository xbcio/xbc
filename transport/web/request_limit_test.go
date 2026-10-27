package web_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

// TestCapRequestBodyBoundsTheReplayOfAReplayableBody pins the upper half of
// the body cap: a request whose GetBody can produce its body again still has
// that replay bounded when the cap is installed, because a replayed read that
// escaped the maximum would hand out exactly the unbounded body the stage
// exists to deny. The replay goes through the request's own GetBody, the way
// a retrying outbound client reads it, rather than through a wrapper the test
// installed itself.
func TestCapRequestBodyBoundsTheReplayOfAReplayableBody(t *testing.T) {
	for _, test := range []struct {
		name          string
		original      string
		replay        string
		wantStatus    int
		wantOriginal  string
		wantReplay    string
		wantOversized bool
	}{
		{
			name:         "both reads fit the cap",
			original:     `{"name":"Alice"}`,
			replay:       `{"name":"Alice"}`,
			wantStatus:   http.StatusOK,
			wantOriginal: `{"name":"Alice"}`,
			wantReplay:   `{"name":"Alice"}`,
		},
		{
			name:          "the replay exceeds the cap the original fit",
			original:      `{"name":"Alice"}`,
			replay:        `{"name":"Alice","email":"alice@example.com","address":{"city":"Shanghai"}}`,
			wantStatus:    http.StatusRequestEntityTooLarge,
			wantOriginal:  `{"name":"Alice"}`,
			wantOversized: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var (
				originalRead string
				replayRead   string
				replayErr    error
			)
			engine := enginetest.New()
			engine.Use(web.CapRequestBody(32, 32))
			engine.POST("/requests", func(_ context.Context, c *web.Ctx) error {
				original, err := io.ReadAll(c.Request().Body)
				if err != nil {
					return web.ParamError(err, nil)
				}
				originalRead = string(original)
				replay, err := c.Request().GetBody()
				if err != nil {
					return web.ParamError(err, nil)
				}
				defer replay.Close()
				replayed, err := io.ReadAll(replay)
				replayErr = err
				replayRead = string(replayed)
				if err != nil {
					return web.ParamError(err, nil)
				}
				return nil
			})

			request := httptest.NewRequest(http.MethodPost, "/requests", strings.NewReader(test.original))
			request.Header.Set("Content-Type", "application/json")
			// httptest.NewRequest records no replay facility, so the test
			// installs the one this case is about: the middleware stacks ahead
			// of a handler set GetBody the same way, and the cap must reach it.
			replaySource := test.replay
			request.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(replaySource)), nil
			}
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)

			assert.Equal(t, test.wantStatus, response.Code)
			assert.Equal(t, test.wantOriginal, originalRead, "the first read must yield the body as sent")
			if !test.wantOversized {
				require.NoError(t, replayErr)
				assert.Equal(t, test.wantReplay, replayRead)
				return
			}
			var maxBytesError *http.MaxBytesError
			require.True(t, errors.As(replayErr, &maxBytesError),
				"the oversized replay must fail with net/http's own MaxBytesError, got %v", replayErr)
			assert.Equal(t, int64(32), maxBytesError.Limit)
		})
	}
}

// TestCapRequestBodyKeepsABodylessRequestBodyless pins the empty-body path: a
// request that arrived with no content still reads as empty after the cap, and
// reading it produces no overflow -- the cap is a boundary, not a payload.
func TestCapRequestBodyKeepsABodylessRequestBodyless(t *testing.T) {
	engine := enginetest.New()
	engine.Use(web.CapRequestBody(32, 32))
	engine.GET("/requests", func(_ context.Context, c *web.Ctx) error {
		body, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return web.ParamError(err, nil)
		}
		assert.Empty(t, body)
		return nil
	})
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/requests", nil))
	assert.Equal(t, http.StatusOK, response.Code)
}

// The limits the route-limit tests run against, in bytes. They are small so a
// test can send a body one byte either side of each boundary, and they are
// ordered so the cases stay distinguishable: a declaration below the default,
// one between the default and the ceiling, and one above the ceiling.
const (
	bodyDefaultLimit   = 16   // web.max_request_body_bytes
	bodyCeiling        = 64   // web.max_request_body_bytes_ceiling where a test sets one
	bodySmallDeclared  = 8    // below the default
	bodyUploadDeclared = 40   // above the default, within the ceiling
	bodyHugeDeclared   = 1024 // above the ceiling
)

// bodyProbe records what a route handler's read of the request body produced,
// so a test can assert on the bytes that reached it and the error that ended
// the read, not only on the status the client saw.
type bodyProbe struct {
	read int
	err  error
}

// newBodyLimitServer starts a Server whose web.max_request_body_bytes is
// bodyDefaultLimit and whose web.max_request_body_bytes_ceiling is ceiling, and
// which serves routes that read their whole body: one that declares nothing, one
// that declares less than the default, one that declares more than the default
// and no more than bodyCeiling, one that declares more than bodyCeiling, and a
// GET-only route that declares more than the default. A ceiling of 0 leaves the
// key unset, which is what a deployment that never heard of it runs with.
func newBodyLimitServer(t *testing.T, ceiling int64) (*web.Server, *bodyProbe) {
	t.Helper()
	probe := &bodyProbe{}
	readBody := func(_ context.Context, c *web.Ctx) error {
		body, err := io.ReadAll(c.Request().Body)
		probe.read, probe.err = len(body), err
		if err != nil {
			return web.ParamError(err, nil)
		}
		c.Status(http.StatusNoContent)
		return nil
	}
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.MaxRequestBodyBytes = bodyDefaultLimit
	cfg.MaxRequestBodyBytesCeiling = ceiling
	server, ctx, _ := newPingServer(t, cfg, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{{
			Identity: plugin.Identity{Plugin: "bodylimit"},
			Value: fakeRouteContributor{register: func(router *web.Router) {
				router.POST("/undeclared", readBody)
				router.POST("/small", readBody).MaxRequestBodyBytes(bodySmallDeclared)
				router.POST("/upload", readBody).MaxRequestBodyBytes(bodyUploadDeclared)
				router.POST("/huge", readBody).MaxRequestBodyBytes(bodyHugeDeclared)
				router.GET("/get-only", readBody).MaxRequestBodyBytes(bodyUploadDeclared)
			}},
		}},
	})
	require.NoError(t, server.Start(ctx))
	// The route table is frozen by OpenTraffic, and the matched route is only
	// recorded once it is: a request served before it would see no CurrentRoute.
	require.NoError(t, server.OpenTraffic(ctx))
	return server, probe
}

// serveBody sends a request of n bytes to path and returns the response. A
// chunked request is one whose length is unknown to the server, which is how a
// client that streams its body arrives: ContentLength is -1 and nothing but the
// reader can discover an overflow.
func serveBody(t *testing.T, server *web.Server, method, path string, n int, chunked bool) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(strings.Repeat("x", n)))
	if chunked {
		request.ContentLength = -1
	}
	response := httptest.NewRecorder()
	testEngineOf(t, server).ServeHTTP(response, request)
	return response
}

func requireBodyTooLarge(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusRequestEntityTooLarge, response.Code)
	assert.Equal(t, "request_body_too_large", decodeProblem(t, response).Properties["code"])
}

// requireReadLimit asserts the read the handler just made ended with net/http's
// own *MaxBytesError carrying exactly limit, which is how a test tells which
// number the reader was built with: the 413 alone says only that some limit was
// exceeded.
func requireReadLimit(t *testing.T, probe *bodyProbe, limit int64) {
	t.Helper()
	var maxBytesError *http.MaxBytesError
	require.True(t, errors.As(probe.err, &maxBytesError), "got %v", probe.err)
	assert.Equal(t, limit, maxBytesError.Limit)
	assert.LessOrEqual(t, int64(probe.read), limit, "no byte past the limit may reach the handler")
}

// TestRouteWithoutADeclarationIsHeldToTheDefaultLimit is the regression half: a
// route that never calls MaxRequestBodyBytes is held to
// web.max_request_body_bytes, and configuring a larger ceiling does not move it.
// That second part is the one a careless merge gets wrong, by treating the
// ceiling as the limit.
func TestRouteWithoutADeclarationIsHeldToTheDefaultLimit(t *testing.T) {
	server, probe := newBodyLimitServer(t, bodyCeiling)

	t.Run("at the default limit", func(t *testing.T) {
		response := serveBody(t, server, http.MethodPost, "/undeclared", bodyDefaultLimit, false)
		assert.Equal(t, http.StatusNoContent, response.Code)
		assert.Equal(t, bodyDefaultLimit, probe.read)
	})
	t.Run("declared length over the default limit and under the ceiling", func(t *testing.T) {
		requireBodyTooLarge(t, serveBody(t, server, http.MethodPost, "/undeclared", bodyDefaultLimit+1, false))
	})
	t.Run("chunked over the default limit and under the ceiling", func(t *testing.T) {
		requireBodyTooLarge(t, serveBody(t, server, http.MethodPost, "/undeclared", bodyDefaultLimit+1, true))
		requireReadLimit(t, probe, bodyDefaultLimit)
	})
}

// TestRouteMayTightenTheDefaultLimit pins the declared-smaller case: the route
// is held to its own limit by the Content-Length check, before its handler runs,
// and only that route -- a sibling that declared nothing still takes a body the
// first one refuses.
func TestRouteMayTightenTheDefaultLimit(t *testing.T) {
	server, probe := newBodyLimitServer(t, bodyCeiling)

	t.Run("at the declared limit", func(t *testing.T) {
		response := serveBody(t, server, http.MethodPost, "/small", bodySmallDeclared, false)
		assert.Equal(t, http.StatusNoContent, response.Code)
		assert.Equal(t, bodySmallDeclared, probe.read)
	})
	t.Run("declared length over the declared limit", func(t *testing.T) {
		probe.read = -1
		requireBodyTooLarge(t, serveBody(t, server, http.MethodPost, "/small", bodySmallDeclared+1, false))
		assert.Equal(t, -1, probe.read, "the 413 must be answered before the route handler runs")
	})
	t.Run("the tightening does not reach a route that declared nothing", func(t *testing.T) {
		response := serveBody(t, server, http.MethodPost, "/undeclared", bodySmallDeclared+1, false)
		assert.Equal(t, http.StatusNoContent, response.Code)
	})
}

// TestRouteMayRaiseTheLimitUpToTheCeiling pins the capability the ceiling exists
// for: a route that declares more than the default is held to what it declared,
// as long as the ceiling allows it. The 17-byte case is the discriminating one,
// since it is over the default and under the declaration, and the sibling that
// declared nothing must still refuse it.
func TestRouteMayRaiseTheLimitUpToTheCeiling(t *testing.T) {
	server, probe := newBodyLimitServer(t, bodyCeiling)

	t.Run("over the default limit and under the declaration", func(t *testing.T) {
		response := serveBody(t, server, http.MethodPost, "/upload", bodyDefaultLimit+1, false)
		assert.Equal(t, http.StatusNoContent, response.Code)
		assert.Equal(t, bodyDefaultLimit+1, probe.read)
	})
	t.Run("at the declared limit", func(t *testing.T) {
		response := serveBody(t, server, http.MethodPost, "/upload", bodyUploadDeclared, false)
		assert.Equal(t, http.StatusNoContent, response.Code)
		assert.Equal(t, bodyUploadDeclared, probe.read)
	})
	t.Run("declared length over the declared limit and under the ceiling", func(t *testing.T) {
		requireBodyTooLarge(t, serveBody(t, server, http.MethodPost, "/upload", bodyUploadDeclared+1, false))
	})
	t.Run("the raise does not reach a route that declared nothing", func(t *testing.T) {
		requireBodyTooLarge(t, serveBody(t, server, http.MethodPost, "/undeclared", bodyDefaultLimit+1, false))
	})
}

// TestChunkedBodyIsHeldToTheRoutesLimitNotAWiderOne pins the case a two-stage
// design gets wrong, in both directions. A chunked request has no Content-Length
// to pre-check, so the reader is the only bound. If the reader were built from
// the default (or the ceiling) while only the length check used the declaration,
// a tightened route would read up to the larger number, and a raised route would
// be cut at the smaller. The reader must carry exactly the route's limit.
func TestChunkedBodyIsHeldToTheRoutesLimitNotAWiderOne(t *testing.T) {
	server, probe := newBodyLimitServer(t, bodyCeiling)

	t.Run("tightened route, between its limit and the default", func(t *testing.T) {
		requireBodyTooLarge(t, serveBody(t, server, http.MethodPost, "/small", bodyDefaultLimit-1, true))
		requireReadLimit(t, probe, bodySmallDeclared)
	})
	t.Run("tightened route, at its limit", func(t *testing.T) {
		response := serveBody(t, server, http.MethodPost, "/small", bodySmallDeclared, true)
		assert.Equal(t, http.StatusNoContent, response.Code)
		assert.Equal(t, bodySmallDeclared, probe.read)
	})
	t.Run("raised route, between the default and its limit", func(t *testing.T) {
		response := serveBody(t, server, http.MethodPost, "/upload", bodyDefaultLimit+1, true)
		assert.Equal(t, http.StatusNoContent, response.Code, "the reader must not be cut at the default")
		assert.Equal(t, bodyDefaultLimit+1, probe.read)
	})
	t.Run("raised route, between its limit and the ceiling", func(t *testing.T) {
		requireBodyTooLarge(t, serveBody(t, server, http.MethodPost, "/upload", bodyCeiling-1, true))
		requireReadLimit(t, probe, bodyUploadDeclared)
	})
}

// TestDeclarationAboveTheCeilingIsCutToTheCeiling pins the bound: a declaration
// the operator's ceiling does not allow is accepted at registration and enforced
// as the ceiling, on the Content-Length check and on the reader alike.
func TestDeclarationAboveTheCeilingIsCutToTheCeiling(t *testing.T) {
	server, probe := newBodyLimitServer(t, bodyCeiling)

	t.Run("at the ceiling", func(t *testing.T) {
		response := serveBody(t, server, http.MethodPost, "/huge", bodyCeiling, false)
		assert.Equal(t, http.StatusNoContent, response.Code)
		assert.Equal(t, bodyCeiling, probe.read)
	})
	t.Run("declared length over the ceiling", func(t *testing.T) {
		requireBodyTooLarge(t, serveBody(t, server, http.MethodPost, "/huge", bodyCeiling+1, false))
	})
	t.Run("chunked over the ceiling", func(t *testing.T) {
		requireBodyTooLarge(t, serveBody(t, server, http.MethodPost, "/huge", bodyCeiling+1, true))
		requireReadLimit(t, probe, bodyCeiling)
	})
}

// TestUnsetCeilingFollowsTheDefaultLimit is the backward-compatibility pin. A
// deployment that never set web.max_request_body_bytes_ceiling has exactly one
// body bound, web.max_request_body_bytes, and no declaration in code may widen
// it: the ceiling resolves to the default limit, so a route that declares more is
// still held to the default. Tightening keeps working, since it was never a
// widening.
func TestUnsetCeilingFollowsTheDefaultLimit(t *testing.T) {
	server, probe := newBodyLimitServer(t, 0)

	t.Run("a declaration above the default is not honoured", func(t *testing.T) {
		response := serveBody(t, server, http.MethodPost, "/upload", bodyDefaultLimit, false)
		assert.Equal(t, http.StatusNoContent, response.Code)
		requireBodyTooLarge(t, serveBody(t, server, http.MethodPost, "/upload", bodyDefaultLimit+1, false))
	})
	t.Run("nor on a chunked body", func(t *testing.T) {
		requireBodyTooLarge(t, serveBody(t, server, http.MethodPost, "/upload", bodyDefaultLimit+1, true))
		requireReadLimit(t, probe, bodyDefaultLimit)
	})
	t.Run("nor a declaration far above it", func(t *testing.T) {
		requireBodyTooLarge(t, serveBody(t, server, http.MethodPost, "/huge", bodyDefaultLimit+1, false))
	})
	t.Run("a route that declared nothing is unchanged", func(t *testing.T) {
		response := serveBody(t, server, http.MethodPost, "/undeclared", bodyDefaultLimit, false)
		assert.Equal(t, http.StatusNoContent, response.Code)
		requireBodyTooLarge(t, serveBody(t, server, http.MethodPost, "/undeclared", bodyDefaultLimit+1, false))
	})
	t.Run("tightening still applies", func(t *testing.T) {
		requireBodyTooLarge(t, serveBody(t, server, http.MethodPost, "/small", bodySmallDeclared+1, false))
	})
}

// TestCeilingBelowTheDefaultLimitFailsStartup pins the validation that keeps the
// two keys coherent. The default is what every undeclared route gets, so a
// ceiling under it would be exceeded by routes that declared nothing at all. The
// error has to name both keys, since either is a fix.
func TestCeilingBelowTheDefaultLimitFailsStartup(t *testing.T) {
	for _, test := range []struct {
		name    string
		ceiling int64
	}{
		{name: "ceiling below the default limit", ceiling: bodyDefaultLimit - 1},
		{name: "negative ceiling", ceiling: -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := web.DefaultConfig()
			cfg.Addr = "127.0.0.1:0"
			cfg.MaxRequestBodyBytes = bodyDefaultLimit
			cfg.MaxRequestBodyBytesCeiling = test.ceiling
			server, ctx, _ := newPingServer(t, cfg, serverInputs{})

			err := server.Start(ctx)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "max_request_body_bytes_ceiling")
			if test.ceiling > 0 {
				assert.Contains(t, err.Error(), "max_request_body_bytes ("+strconv.Itoa(bodyDefaultLimit)+")",
					"the error must name the default limit it conflicts with and its value")
			}
		})
	}
}

// TestUnmatchedRequestsAreHeldToTheDefaultLimit pins 404 and 405: neither has a
// current route, and neither may skip the check for that reason. No route's
// declaration may leak to them either -- a 405 on a path whose GET route
// declared more than the default is still a request no route matched, and so is
// a PUT to a path only POST serves. They are held to the default rather than to
// the ceiling, since no route asked for more on their behalf.
func TestUnmatchedRequestsAreHeldToTheDefaultLimit(t *testing.T) {
	server, _ := newBodyLimitServer(t, bodyCeiling)

	for _, test := range []struct {
		name   string
		method string
		path   string
		status int
	}{
		{name: "not found", method: http.MethodPost, path: "/missing", status: http.StatusNotFound},
		{name: "method not allowed beside a route that declared more", method: http.MethodPost, path: "/get-only", status: http.StatusMethodNotAllowed},
		{name: "method not allowed on a path another method raised", method: http.MethodPut, path: "/upload", status: http.StatusMethodNotAllowed},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.NotPanics(t, func() {
				assert.Equal(t, test.status, serveBody(t, server, test.method, test.path, bodyDefaultLimit, false).Code,
					"a body at the default limit must still reach the 404 or 405")
				requireBodyTooLarge(t, serveBody(t, server, test.method, test.path, bodyDefaultLimit+1, false))
				assert.Equal(t, test.status, serveBody(t, server, test.method, test.path, bodyDefaultLimit, true).Code,
					"a chunked body reaches the terminal handler, which never reads it")
			})
		})
	}
}

// TestRouteLimitBoundsTheReplayOfTheBody pins that the wrapper capRequestBody puts
// on Request.GetBody carries the route's limit, not the default or the ceiling:
// a replay that fits the original's limit but not the route's is the case that
// would escape if the two were different numbers, and a raised route's replay
// must not be cut at the default.
func TestRouteLimitBoundsTheReplayOfTheBody(t *testing.T) {
	var (
		original  string
		replayed  string
		replayErr error
	)
	replayRoute := func(_ context.Context, c *web.Ctx) error {
		first, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return web.ParamError(err, nil)
		}
		original = string(first)
		replay, err := c.Request().GetBody()
		if err != nil {
			return web.ParamError(err, nil)
		}
		defer replay.Close()
		second, err := io.ReadAll(replay)
		replayed, replayErr = string(second), err
		if err != nil {
			return web.ParamError(err, nil)
		}
		c.Status(http.StatusNoContent)
		return nil
	}
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.MaxRequestBodyBytes = bodyDefaultLimit
	cfg.MaxRequestBodyBytesCeiling = bodyCeiling
	server, ctx, _ := newPingServer(t, cfg, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{{
			Identity: plugin.Identity{Plugin: "bodyreplay"},
			Value: fakeRouteContributor{register: func(router *web.Router) {
				router.POST("/replay-small", replayRoute).MaxRequestBodyBytes(bodySmallDeclared)
				router.POST("/replay-upload", replayRoute).MaxRequestBodyBytes(bodyUploadDeclared)
			}},
		}},
	})
	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))

	send := func(path, first, replay string) *httptest.ResponseRecorder {
		original, replayed, replayErr = "", "", nil
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(first))
		request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(replay)), nil }
		response := httptest.NewRecorder()
		testEngineOf(t, server).ServeHTTP(response, request)
		return response
	}
	of := func(n int) string { return strings.Repeat("a", n) }

	t.Run("tightened route, replay within its limit", func(t *testing.T) {
		response := send("/replay-small", of(bodySmallDeclared), of(bodySmallDeclared))
		assert.Equal(t, http.StatusNoContent, response.Code)
		assert.Equal(t, of(bodySmallDeclared), replayed)
	})
	t.Run("tightened route, replay over its limit and under the default", func(t *testing.T) {
		requireBodyTooLarge(t, send("/replay-small", of(bodySmallDeclared), of(bodySmallDeclared+1)))
		assert.Equal(t, of(bodySmallDeclared), original, "the first read must yield the body as sent")
		var maxBytesError *http.MaxBytesError
		require.True(t, errors.As(replayErr, &maxBytesError), "got %v", replayErr)
		assert.Equal(t, int64(bodySmallDeclared), maxBytesError.Limit)
	})
	t.Run("raised route, replay over the default and within its limit", func(t *testing.T) {
		response := send("/replay-upload", of(bodyDefaultLimit+1), of(bodyUploadDeclared))
		assert.Equal(t, http.StatusNoContent, response.Code, "the replay must not be cut at the default")
		assert.Equal(t, of(bodyUploadDeclared), replayed)
	})
	t.Run("raised route, replay over its limit and under the ceiling", func(t *testing.T) {
		requireBodyTooLarge(t, send("/replay-upload", of(bodyDefaultLimit+1), of(bodyUploadDeclared+1)))
		var maxBytesError *http.MaxBytesError
		require.True(t, errors.As(replayErr, &maxBytesError), "got %v", replayErr)
		assert.Equal(t, int64(bodyUploadDeclared), maxBytesError.Limit)
	})
}

// TestMaxRequestBodyBytesIsRecordedOnEveryRowAndRejectsNonPositiveLimits covers
// the declaration itself: it reaches every row a Match registers like Perm and
// Auth do, a later call overwrites, and a limit that is not positive panics at
// the registration site rather than reading as "undeclared". The recorded value
// is the declared one even when it exceeds any ceiling: the cut happens when the
// request is served, so the route table keeps saying what the route asked for.
func TestMaxRequestBodyBytesIsRecordedOnEveryRowAndRejectsNonPositiveLimits(t *testing.T) {
	_, router := newTestEngineAndRouter("/api")
	noop := func(context.Context, *web.Ctx) error { return nil }
	router.Match([]string{http.MethodPost, http.MethodPut}, "/upload", noop).MaxRequestBodyBytes(1 << 30).MaxRequestBodyBytes(512)
	router.POST("/huge", noop).MaxRequestBodyBytes(1 << 40)
	router.POST("/plain", noop)
	late := router.POST("/late", noop)
	catalog, err := router.Freeze()
	require.NoError(t, err)

	for _, method := range []string{http.MethodPost, http.MethodPut} {
		info, ok := catalog.Lookup(method, "/api/upload")
		require.True(t, ok)
		assert.Equal(t, int64(512), info.MaxRequestBodyBytes, method)
	}
	huge, ok := catalog.Lookup(http.MethodPost, "/api/huge")
	require.True(t, ok)
	assert.Equal(t, int64(1<<40), huge.MaxRequestBodyBytes, "a declaration is recorded as declared")
	plain, ok := catalog.Lookup(http.MethodPost, "/api/plain")
	require.True(t, ok)
	assert.Zero(t, plain.MaxRequestBodyBytes, "a route that declared nothing records nothing")

	for _, limit := range []int64{0, -1} {
		_, router := newTestEngineAndRouter("/api")
		route := router.POST("/upload", noop)
		assert.PanicsWithValue(t,
			fmt.Sprintf("xbc: Route.MaxRequestBodyBytes requires a positive limit, got %d\n  → an undeclared route is held to web.max_request_body_bytes; pass the number of bytes this route accepts, or omit the call", limit),
			func() { route.MaxRequestBodyBytes(limit) })
	}

	assert.Panics(t, func() { late.MaxRequestBodyBytes(1) },
		"declaring after the route table is frozen must panic like every other route declaration")
}
