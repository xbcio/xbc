// Package swagger generates an OpenAPI 3 document from XBC's frozen Web route
// catalog and serves both the JSON document and an embedded Swagger UI.
//
// # Usage
//
// Compose Swagger explicitly with the Web transport. Ordinary imports and
// Bundle are side-effect free:
//
//	import (
//		"github.com/xbcio/xbc"
//		webprelude "github.com/xbcio/xbc/transport/web/prelude"
//		"github.com/xbcio/xbc/transport/web/integrations/swagger"
//	)
//
//	func newApp() (*xbc.App, error) {
//		return xbc.New(xbc.WithBundles(
//			webprelude.Bundle(),
//			swagger.Bundle(),
//		))
//	}
//
// A plugins.swagger section activates the composed Definition and configures
// the documentation routes:
//
//	plugins:
//	  swagger:
//	    title: Orders API
//	    version: v1
//	    json_path: /openapi.json
//	    ui_path: /docs
//	    ui_enabled: true
//	    bearer_auth: true
//
// Route metadata becomes OpenAPI operation metadata. Named public routes omit
// bearer security; Perm and Idempotent become x-xbc-* extensions:
//
//	type Users struct{}
//
//	func (*Users) RegisterRoutes(r *web.Router) {
//		r.GET("/users/:id", func(c *gin.Context) {
//			c.Status(http.StatusNotImplemented)
//		}).
//			Name("users.get").
//			Auth(web.Public()).
//			Perm("users:read").
//			Idempotent()
//	}
//
//	var _ web.RouteContributor = (*Users)(nil)
//
// Gin :parameters are emitted as OpenAPI {parameters}. Duplicate operation IDs
// fail traffic preparation rather than publishing an ambiguous document. The
// Swagger primary exports both web.RouteContributor and
// web.RouteCatalogListener: it registers its public endpoints first, then builds
// the document after Web freezes the complete route table. Document returns a
// defensive copy of that snapshot. Every operation documents its default error
// response as application/problem+json referencing
// components.schemas.ProblemDetail, including structured validation field
// errors. Executables using xbc.Run may opt into process-global composition
// through the leaf autoload adapter.
package swagger
