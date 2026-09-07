// Package rbac adapts the protocol-neutral security/rbac Manager to explicit
// Gin route and group middleware.
//
// RequireAll and RequireAny authorize only the verified web.CurrentPrincipal.
// An anonymous or denied request receives an RFC 9457 forbidden response, while
// Manager failures flow through web.AbortError and its safe error boundary.
// This package owns no plugin Definition, configuration, backend, lifecycle, or
// autoload behavior; applications compose security/rbac separately.
//
// # Usage
//
//	func mount(router *gin.Engine, manager businessrbac.Manager) {
//		settings := router.Group("/settings")
//		settings.Use(webrbac.RequireAll(manager,
//			businessrbac.Permission{Object: "settings.audit", Action: "read"},
//		))
//	}
package rbac
