// Package rbac adapts the protocol-neutral extensions/authorization/rbac Manager to explicit
// Web route and group middleware.
//
// RequireAll and RequireAny authorize only the verified web.CurrentPrincipal.
// An anonymous or denied request receives an RFC 9457 forbidden response, while
// Manager failures flow through web.AbortError and its safe error boundary.
// This package owns no plugin Definition, configuration, backend, lifecycle, or
// autoload behavior; applications compose extensions/authorization/rbac separately.
//
// # Usage
//
// Scope the middleware by declaring it on the group, so every route registered
// on that sub-router is covered:
//
//	func (*Settings) RegisterRoutes(r *web.Router) {
//		settings := r.Group("/settings", webrbac.RequireAll(manager,
//			businessrbac.Permission{Object: "settings.audit", Action: "read"},
//		))
//		settings.GET("/audit", handleAudit).Name("settings.audit.read")
//	}
//
// Register through *web.Router rather than the underlying *gin.Engine: only
// the former records a RouteInfo, and a route missing from that table is
// invisible to the authentication policy and to every RouteCatalogListener.
package rbac
