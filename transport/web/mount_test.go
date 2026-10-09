package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// TestMountDispatchesTheSubtreeWithThePrefixStripped pins the path contract an
// external handler is written against: the mounted prefix is removed with
// http.StripPrefix's own semantics, so the prefix itself arrives with an empty
// path and a request inside the subtree with its remainder -- while a path that
// only shares the prefix's characters, or stops one segment short, never
// reaches the handler at all. The 404 cases are what keep a mount from being a
// prefix-string match: it covers the subtree on a segment boundary.
func TestMountDispatchesTheSubtreeWithThePrefixStripped(t *testing.T) {
	engine, router := newTestEngineAndRouter("/api")

	var seen []string
	router.Mount("/flow", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	_, err := router.Freeze()
	require.NoError(t, err)

	cases := []struct {
		name     string
		method   string
		target   string
		wantCode int
		wantRan  bool
		wantPath string // the prefix-stripped path the handler must see; read only when wantRan
	}{
		{name: "the prefix itself", method: http.MethodGet, target: "/api/flow", wantCode: http.StatusNoContent, wantRan: true},
		{name: "the subtree root", method: http.MethodGet, target: "/api/flow/", wantCode: http.StatusNoContent, wantRan: true, wantPath: "/"},
		{name: "one segment below", method: http.MethodGet, target: "/api/flow/status", wantCode: http.StatusNoContent, wantRan: true, wantPath: "/status"},
		{name: "a POST deeper in the subtree", method: http.MethodPost, target: "/api/flow/a/b", wantCode: http.StatusNoContent, wantRan: true, wantPath: "/a/b"},
		{name: "a sibling sharing the prefix's characters", method: http.MethodGet, target: "/api/flowx", wantCode: http.StatusNotFound},
		{name: "a prefix one segment short", method: http.MethodGet, target: "/api/flo", wantCode: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(seen)
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.target, nil))

			assert.Equal(t, tc.wantCode, recorder.Code)
			if !tc.wantRan {
				assert.Len(t, seen, before, "the mounted handler must not run for a path outside the subtree")
				return
			}
			require.Len(t, seen, before+1, "the mounted handler must run exactly once")
			assert.Equal(t, tc.wantPath, seen[before], "the handler must see the request with the prefix stripped")
		})
	}
}

// TestMountReportsTheMountRowAndPreservesTheOriginalPath pins what the rest of
// the framework sees around a mounted request, from both sides of the handler:
// CurrentRoute answers with the mount's own frozen row before global middleware
// has called Next -- which is what puts a mounted request under the same
// authentication and authorization resolution as any other route -- and the
// request every framework middleware observes keeps its original path, before
// and after the handler, even though the handler itself was handed a stripped
// copy. A mount that stripped the URL in place would corrupt access logs and
// metrics for every later handler, and one that skipped recordCurrentRoute
// would leave the whole subtree reading as an unmatched request.
func TestMountReportsTheMountRowAndPreservesTheOriginalPath(t *testing.T) {
	engine, router := newTestEngineAndRouter("/api")

	var route web.RouteInfo
	var matchedBeforeNext bool
	var pathBeforeNext, pathAfterNext string
	router.AppendGlobalHandler(func(_ context.Context, c *web.Ctx) error {
		route, matchedBeforeNext = web.CurrentRoute(c)
		pathBeforeNext = c.Request().URL.Path
		c.Next()
		pathAfterNext = c.Request().URL.Path
		return nil
	})

	var handlerPath string
	router.Mount("/flow", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	_, err := router.Freeze()
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/flow/status", nil))

	assert.Equal(t, http.StatusNoContent, recorder.Code)
	require.True(t, matchedBeforeNext, "global middleware must see the mount row before it calls Next")
	assert.Equal(t, web.RouteInfo{Method: http.MethodGet, Path: "/api/flow", Mounted: true}, route,
		"the row must name the prefix and be marked mounted")
	assert.Equal(t, "/status", handlerPath, "the handler must receive the prefix-stripped path")
	assert.Equal(t, "/api/flow/status", pathBeforeNext, "framework middleware must observe the original request path")
	assert.Equal(t, "/api/flow/status", pathAfterNext, "stripping must not rewrite the request in place")
}

// TestMountAnswersUnmountedMethodsWith405AndAllow pins the method dimension of
// a mount: the subtree is mounted for the methods the port covers and no
// others, so a TRACE or CONNECT request inside it is the framework's 405 with
// the mounted methods listed in Allow -- the same answer an unmounted path
// would get -- rather than the handler's own decision. Allow is compared as a
// set: the port leaves the order to the engine.
func TestMountAnswersUnmountedMethodsWith405AndAllow(t *testing.T) {
	engine, router := newTestEngineAndRouter("/api")

	var ran bool
	router.Mount("/flow", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ran = true
		w.WriteHeader(http.StatusNoContent)
	}))
	_, err := router.Freeze()
	require.NoError(t, err)

	for _, method := range []string{http.MethodTrace, http.MethodConnect} {
		t.Run(method, func(t *testing.T) {
			ran = false
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(method, "/api/flow/status", nil))

			assert.Equal(t, http.StatusMethodNotAllowed, recorder.Code)
			assert.False(t, ran, "an unmounted method must not reach the mounted handler")
			// Result().Header is the response as it was committed, not the
			// recorder's live map: an Allow set after the status line never
			// reaches the client, and the live map cannot tell the two apart.
			allowed := make(map[string]bool)
			for _, entry := range strings.Split(recorder.Result().Header.Get("Allow"), ",") {
				allowed[strings.TrimSpace(entry)] = true
			}
			for _, want := range []string{
				http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
				http.MethodHead, http.MethodOptions, http.MethodDelete,
			} {
				assert.True(t, allowed[want], "Allow must list %s", want)
			}
			assert.False(t, allowed[method], "Allow must not list the request's own method")
		})
	}
}

// TestMountedSubtreeResolvesThroughTheSecurityDefault pins that mount rows
// really participate in authentication resolution, and that the documented
// asymmetry holds: a request inside a mounted subtree resolves against the
// mount row, so the restrictive default applies wherever nothing is declared,
// a route-level .Auth declaration works exactly as it does for a plain route,
// and an application-level wildcard rule naming the prefix reaches the whole
// subtree. A rule naming a path inside the subtree cannot be covered here --
// there is no row for it, which is precisely why the prefix is what a rule has
// to name.
func TestMountedSubtreeResolvesThroughTheSecurityDefault(t *testing.T) {
	engine, router := newTestEngineAndRouter("/api")

	middleware, err := web.NewAuthenticationMiddleware(
		web.SecurityConfig{
			Default:  web.SecurityDeny,
			Policies: []web.PolicyRule{{Match: "GET /api/flow/**", Permit: true}},
		},
		[]plugin.Entry[authentication.Authenticator]{
			{Identity: plugin.Identity{Plugin: "jwt"}, Value: &stubAuth{scheme: "jwt", result: authentication.Rejected("unused")}},
		},
		[]plugin.Entry[web.CredentialExtractor]{
			{Identity: plugin.Identity{Plugin: "jwt"}, Value: &stubExtractor{scheme: "jwt", result: authentication.AbsentWithChallenge(`Bearer realm="api"`)}},
		},
	)
	require.NoError(t, err)
	router.AppendGlobalHandler(middleware.Handler())

	var ran bool
	newHandler := func() http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			ran = true
			w.WriteHeader(http.StatusNoContent)
		})
	}
	router.Mount("/flow", newHandler())   // covered by the application-level rule
	router.Mount("/closed", newHandler()) // nothing declared: the default decides
	router.Mount("/open", newHandler()).Auth(web.Public())

	catalog, err := router.Freeze()
	require.NoError(t, err)
	require.NoError(t, middleware.RoutesReady(catalog))

	cases := []struct {
		name     string
		target   string
		wantCode int
		wantRan  bool
	}{
		{name: "an application-level wildcard rule naming the prefix", target: "/api/flow/status", wantCode: http.StatusNoContent, wantRan: true},
		{name: "a route-level public declaration", target: "/api/open/status", wantCode: http.StatusNoContent, wantRan: true},
		{name: "the restrictive default", target: "/api/closed/status", wantCode: http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ran = false
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.target, nil))

			assert.Equal(t, tc.wantCode, recorder.Code)
			assert.Equal(t, tc.wantRan, ran, "whether the mounted handler runs must follow the resolved policy")
			if tc.wantCode == http.StatusUnauthorized {
				assert.NotEmpty(t, recorder.Header().Get("WWW-Authenticate"),
					"a rejected request must carry the extractor's challenge")
			}
		})
	}
}
