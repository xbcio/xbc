package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
)

// noopRouteHandler satisfies a registration with no behaviour to observe.
func noopRouteHandler(context.Context, *web.Ctx) error { return nil }

// TestRouteMetaReachesEveryRowAHandleCovers guards the property that makes
// metadata part of a route declaration rather than a side map: Any, Match, and
// Mount each expand one registration into several method rows, and Meta writes
// into all of them. A Meta that updated only the first row would leave the
// other methods without the plugin data while looking correctly declared at
// the call site. The second Meta call pins overwrite semantics: metadata is a
// declaration, not an accumulation.
func TestRouteMetaReachesEveryRowAHandleCovers(t *testing.T) {
	t.Parallel()

	_, router := newTestEngineAndRouter("/api")
	router.Any("/resource", noopRouteHandler).
		Meta("openapi.summary", "resource").
		Meta("openapi.summary", "resource, revised")
	router.Match([]string{http.MethodGet, http.MethodPost}, "/report", noopRouteHandler).
		Meta("openapi.summary", "report")
	router.Mount("/flow", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		Meta("openapi.summary", "flow")

	catalog, err := router.Freeze()
	require.NoError(t, err)

	for _, method := range web.AnyMethods {
		info, ok := catalog.Lookup(method, "/api/resource")
		require.Truef(t, ok, "Any did not register %s", method)
		assert.Equalf(t, "resource, revised", info.Metadata["openapi.summary"],
			"Any's handle did not carry the metadata -- or the later overwrite -- to %s", method)
	}

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		info, ok := catalog.Lookup(method, "/api/report")
		require.Truef(t, ok, "Match did not register %s", method)
		assert.Equalf(t, "report", info.Metadata["openapi.summary"],
			"Match's handle did not carry the metadata to %s", method)
	}

	for _, method := range web.AnyMethods {
		info, ok := catalog.Lookup(method, "/api/flow")
		require.Truef(t, ok, "Mount did not register %s", method)
		assert.Equalf(t, "flow", info.Metadata["openapi.summary"],
			"Mount's handle did not carry the metadata to %s", method)
	}
}

// TestRouteMetaReadsTheExactDynamicType pins RouteMeta's assertion rule. The
// wrong-type half is what a value stored in an any bag can silently get wrong:
// Go type assertions do not convert, so an int read as int64 must report
// absence rather than a converted value.
func TestRouteMetaReadsTheExactDynamicType(t *testing.T) {
	t.Parallel()

	_, router := newTestEngineAndRouter("/")
	router.GET("/orders", noopRouteHandler).
		Meta("openapi.summary", "list orders").
		Meta("openapi.deprecated", true).
		Meta("openapi.weight", 7)

	catalog, err := router.Freeze()
	require.NoError(t, err)
	info, ok := catalog.Lookup(http.MethodGet, "/orders")
	require.True(t, ok)

	summary, ok := web.RouteMeta[string](info, "openapi.summary")
	assert.True(t, ok)
	assert.Equal(t, "list orders", summary)

	deprecated, ok := web.RouteMeta[bool](info, "openapi.deprecated")
	assert.True(t, ok)
	assert.True(t, deprecated)

	weight, ok := web.RouteMeta[int](info, "openapi.weight")
	assert.True(t, ok)
	assert.Equal(t, 7, weight)

	missing, ok := web.RouteMeta[string](info, "openapi.missing")
	assert.False(t, ok, "a missing key must report absence rather than a zero value")
	assert.Empty(t, missing)

	wrongType, ok := web.RouteMeta[int64](info, "openapi.weight")
	assert.False(t, ok, "Go type assertions do not convert: an int is not readable as int64")
	assert.Zero(t, wrongType)

	anyValue, ok := web.RouteMeta[any](info, "openapi.weight")
	assert.True(t, ok, "T = any must read any non-nil value")
	assert.Equal(t, 7, anyValue)
}

// TestCurrentRouteReportsRouteMetadataDuringARequest covers the request-time
// view, including a mount row: CurrentRoute resolves through the frozen index,
// which is the only route view a handler or middleware can reach.
func TestCurrentRouteReportsRouteMetadataDuringARequest(t *testing.T) {
	t.Parallel()

	engine, router := newTestEngineAndRouter("/api")
	var current web.RouteInfo
	router.AppendGlobalHandler(func(_ context.Context, c *web.Ctx) error {
		current, _ = web.CurrentRoute(c)
		return nil
	})
	router.GET("/orders", noopRouteHandler).Meta("openapi.summary", "list orders")
	router.Mount("/flow", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		Meta("openapi.summary", "flow subtree")

	_, err := router.Freeze()
	require.NoError(t, err)

	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/orders", nil))
	summary, ok := web.RouteMeta[string](current, "openapi.summary")
	require.True(t, ok, "CurrentRoute must expose the matched row's metadata")
	assert.Equal(t, "list orders", summary)

	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/flow/status", nil))
	require.True(t, current.Mounted, "the dispatched request must match the mount row")
	summary, ok = web.RouteMeta[string](current, "openapi.summary")
	require.True(t, ok, "a mount row must expose its metadata to CurrentRoute")
	assert.Equal(t, "flow subtree", summary)
}

// TestCurrentRouteReturnsItsOwnMetadataMap covers the read-side copy:
// recordCurrentRoute stores a clone and CurrentRoute clones again on every
// read, so a handler that mutates what it was handed cannot change what the
// next reader -- in the same request or another -- observes.
func TestCurrentRouteReturnsItsOwnMetadataMap(t *testing.T) {
	t.Parallel()

	engine, router := newTestEngineAndRouter("/api")
	var first, second string
	router.GET("/orders", func(_ context.Context, c *web.Ctx) error {
		info, _ := web.CurrentRoute(c)
		first, _ = web.RouteMeta[string](info, "openapi.summary")
		info.Metadata["openapi.summary"] = "mutated by caller"
		again, _ := web.CurrentRoute(c)
		second, _ = web.RouteMeta[string](again, "openapi.summary")
		return nil
	}).Meta("openapi.summary", "list orders")

	_, err := router.Freeze()
	require.NoError(t, err)

	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/orders", nil))
	assert.Equal(t, "list orders", first)
	assert.Equal(t, "list orders", second,
		"mutating a map CurrentRoute returned must not reach the next CurrentRoute read")
}

// TestRouteMetaRejectsAnEmptyKey pins the registration-site refusal. An empty
// key cannot be namespaced or found deliberately, so accepting it would only
// hide a mistake until a consumer silently read nothing.
func TestRouteMetaRejectsAnEmptyKey(t *testing.T) {
	t.Parallel()

	_, router := newTestEngineAndRouter("/")
	route := router.GET("/orders", noopRouteHandler)

	assert.PanicsWithValue(t,
		"xbc: Route.Meta requires a non-empty key\n  → an empty key cannot be namespaced or found deliberately; name the data after its producer, for example \"openapi.summary\"",
		func() { route.Meta("", "value") },
	)
}

// TestRouteMetaRejectsANilValue pins the other registration-site refusal: a nil
// any is present in the map but can never satisfy RouteMeta's type assertion,
// so accepting it would make a declared entry read as absent.
func TestRouteMetaRejectsANilValue(t *testing.T) {
	t.Parallel()

	_, router := newTestEngineAndRouter("/")
	route := router.GET("/orders", noopRouteHandler)

	assert.PanicsWithValue(t,
		"xbc: Route.Meta requires a non-nil value for key \"openapi.summary\"\n  → a nil any is present in the map but can never satisfy RouteMeta's type assertion, so it would read as absent; pass a typed value, or omit the call",
		func() { route.Meta("openapi.summary", nil) },
	)
}

// TestRouteMetaCannotChangeAfterFreeze keeps metadata under the same freeze
// guard as every other route declaration: a row the dispatcher serves must not
// change underneath the catalog a listener already received.
func TestRouteMetaCannotChangeAfterFreeze(t *testing.T) {
	t.Parallel()

	_, router := newTestEngineAndRouter("/")
	route := router.GET("/orders", noopRouteHandler)
	_, err := router.Freeze()
	require.NoError(t, err)

	assert.PanicsWithValue(t,
		"xbc: route table is frozen, RouteCatalogListener phase cannot change route metadata",
		func() { route.Meta("openapi.summary", "list orders") },
	)
}

// TestRouteWithoutMetadataKeepsANilMap is the no-extra-cost path: Meta
// allocates lazily, so a composition that declares no metadata carries no map
// in any of its rows.
func TestRouteWithoutMetadataKeepsANilMap(t *testing.T) {
	t.Parallel()

	_, router := newTestEngineAndRouter("/")
	router.GET("/orders", noopRouteHandler)

	catalog, err := router.Freeze()
	require.NoError(t, err)
	info, ok := catalog.Lookup(http.MethodGet, "/orders")
	require.True(t, ok)

	assert.Nil(t, info.Metadata, "a route that never calls Meta must not allocate a metadata map")
}
