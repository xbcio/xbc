package web

// RouteContributor registers routes before the server freezes its route table.
// Each contributing Plugin exports this contract directly; no provider slice or
// runtime capability scan is involved.
type RouteContributor interface {
	RegisterRoutes(*Router)
}

// RouteCatalogListener observes the immutable route table exactly once, at the
// moment the server freezes it: while preparing traffic for a boot, or in the
// validate command's Preflight, which freezes the same table for a process that
// will never serve. An error keeps the runtime traffic gate closed and triggers
// normal lifecycle unwind.
type RouteCatalogListener interface {
	RoutesReady(RouteCatalog) error
}
