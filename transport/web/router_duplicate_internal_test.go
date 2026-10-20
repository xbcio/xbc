package web

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleRejectsDuplicateRouteOnServingPlane pins the diagnostic the
// duplicate check exists for: gin would refuse the second registration too,
// but its panic names neither route, so the author cannot tell which of two
// registrations to remove. Handle refuses the exact duplicate first and names
// the existing route by its own name and plane.
func TestHandleRejectsDuplicateRouteOnServingPlane(t *testing.T) {
	router, _ := newTestRouter("/api")
	router.GET("/orders", noopRoute).Name("orders.list")

	assert.PanicsWithValue(t,
		"xbc: route GET /api/orders is already registered by route \"orders.list\" on the serving plane\n"+
			"  → one method and path is served by one handler; remove one of the two registrations",
		func() { router.GET("/orders", noopRoute) })
}

// TestHandleRejectsDuplicateRouteWithoutAName keeps the diagnostic useful
// when the existing registration never declared a name: method, path, and
// plane still identify it, and the guidance still says what to do.
func TestHandleRejectsDuplicateRouteWithoutAName(t *testing.T) {
	router, _ := newTestRouter("/api")
	router.GET("/orders", noopRoute)

	assert.PanicsWithValue(t,
		"xbc: route GET /api/orders is already registered on the serving plane\n"+
			"  → one method and path is served by one handler; remove one of the two registrations",
		func() { router.GET("/orders", noopRoute) })
}

// TestHandleRejectsDuplicatePatternRoute covers the case conflictingRegistration
// deliberately skips: two identical pattern paths. No mount is involved and the
// engine's pattern syntax is irrelevant, because the two path strings are equal
// -- which is exactly what the duplicate check compares.
func TestHandleRejectsDuplicatePatternRoute(t *testing.T) {
	router, _ := newTestRouter("/api")
	router.GET("/orders/:id", noopRoute)

	assert.PanicsWithValue(t,
		"xbc: route GET /api/orders/:id is already registered on the serving plane\n"+
			"  → one method and path is served by one handler; remove one of the two registrations",
		func() { router.GET("/orders/:id", noopRoute) })
}

// TestHandleRejectsDuplicateRouteOnManagementPlane pins the plane named in the
// message. The two planes share one route table, so an "already registered"
// that did not say which plane would leave the author guessing which listener
// owns the existing entry.
func TestHandleRejectsDuplicateRouteOnManagementPlane(t *testing.T) {
	router, _, _ := newManagedTestRouter("/api")
	router.Management().GET("/metrics", noopRoute).Name("metrics")

	assert.PanicsWithValue(t,
		"xbc: route GET /api/metrics is already registered by route \"metrics\" on the management plane\n"+
			"  → one method and path is served by one handler; remove one of the two registrations",
		func() { router.Management().GET("/metrics", noopRoute) })
}

// TestHandleAcceptsDistinctRegistrations is the positive control for the
// duplicate check: the method and the path are each part of the identity, so
// varying either one is a distinct registration. Without this, a check that
// refused too much -- say, any second route on a path -- would still pass the
// panic tests above.
func TestHandleAcceptsDistinctRegistrations(t *testing.T) {
	router, _ := newTestRouter("/api")

	require.NotPanics(t, func() {
		router.GET("/orders/list", noopRoute) // same method, longer literal path
		router.GET("/orders", noopRoute)      // its prefix: still a distinct path
		router.POST("/orders", noopRoute)     // same path, different method
		router.GET("/orders/:id", noopRoute)  // same method, different pattern path
	})
	require.Len(t, *router.routes, 4)

	_, err := router.freeze()
	require.NoError(t, err, "distinct registrations must freeze")
}

// TestHandleDefersCrossPlaneDuplicateToFreeze pins the boundary between the two
// checks. The same method and path on both planes is not an engine duplicate --
// each plane registers into its own engine, so neither engine sees it -- and
// Handle must not preempt it: freeze owns that verdict, through
// validateRouteScopes, and its message names both planes.
func TestHandleDefersCrossPlaneDuplicateToFreeze(t *testing.T) {
	router, _, _ := newManagedTestRouter("/")

	require.NotPanics(t, func() {
		router.GET("/metrics", noopRoute)
		router.Management().GET("/metrics", noopRoute)
	}, "a cross-plane duplicate is the route table's collision, not Handle's: freeze must report it")

	_, err := router.freeze()
	require.EqualError(t, err,
		"xbc: route GET /metrics is registered on both the serving plane and the management plane\n"+
			"  → the route table holds one entry per method and path, so the two registrations would share an entry and a request would be reported under whichever plane registered last; register the route on exactly one plane")
}
