package web

// RouteContributor registers routes before the server freezes its route table.
// Each contributing Plugin exports this contract directly; no provider slice or
// runtime capability scan is involved.
type RouteContributor interface {
	RegisterRoutes(*Router)
}

// RouteCatalogListener observes the immutable route table exactly once during
// the server's traffic-preparation stage. An error keeps the runtime traffic
// gate closed and triggers normal lifecycle unwind.
type RouteCatalogListener interface {
	RoutesReady(RouteCatalog) error
}
