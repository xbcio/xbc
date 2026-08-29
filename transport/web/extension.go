package web

// MiddlewareProvider is a capability a plugin implements to contribute
// entries to the HTTP middleware chain. (*Server).Start discovers every
// instance implementing this via plugin.Extensions[MiddlewareProvider],
// qualifies each Middleware.Name with the providing Extension.Identity
// (middleware_order.go's qualify), then orders the combined set with that file's
// orderMiddlewares before installing them on the gin.Engine.
type MiddlewareProvider interface{ Middlewares() []Middleware }

// RouteProvider is a capability a plugin implements to register routes.
// (*Server).Start discovers every instance implementing this via
// plugin.Extensions[RouteProvider] and calls RegisterRoutes once per
// instance, in core's dependency-topological order, before freezing the
// route table.
type RouteProvider interface{ RegisterRoutes(r *Router) }

// RouteCatalogConsumer is a capability a plugin implements to observe the
// frozen route table once assembly is done -- swagger generation, a policy
// sync that needs every route at once, and similar use cases. It replaces
// the pre-split kernel's PostRouter: that interface's PostRoutes(ctx) forced
// a plugin to reach back into Context.Routes() for the table itself,
// keeping a live coupling between "the route table exists" and "Context
// knows how to fetch it". RoutesReady is handed the table directly instead,
// so plugin.Context never needs a Routes accessor at all (design §5.7).
type RouteCatalogConsumer interface {
	RoutesReady(routes RouteCatalog) error
}
