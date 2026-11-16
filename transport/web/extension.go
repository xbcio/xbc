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
//
// The resolver alongside the catalog is the application's compiled
// authentication policy, exposing the same resolution the request path
// enforces through PolicyResolver.Resolve. It is read-only, immutable for the
// lifetime of the process, and safe for concurrent use.
type RouteCatalogListener interface {
	RoutesReady(RouteCatalog, PolicyResolver) error
}

// RouteAuthorizer marks a contributed middleware that enforces the permission
// a route declares through Route.Perm. The marker is a declaration, not a
// capability probe: a middleware that implements it says every request that
// reaches it is checked against the permission of the matched route.
type RouteAuthorizer interface {
	AuthorizesRoutes()
}
