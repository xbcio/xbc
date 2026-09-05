// Package casbin provides route-aware authorization for XBC's Web transport.
//
// By default a protected route is authorized with its RouteInfo.Perm value and
// the verified subject published through web.SetPrincipal. Applications can
// instead select the path_method convention or inject a SubjectResolver for a
// different, already-verified identity source. This package never parses
// credentials or unverified bearer tokens itself.
//
// # Usage
//
// Mark protected routes with permissions and grant those permissions in the
// Casbin policy. The secure default denies a protected route whose permission
// is missing:
//
//	func (*Orders) RegisterRoutes(r *web.Router) {
//		r.GET("/orders", func(c *gin.Context) {
//			c.Status(http.StatusNoContent)
//		}).Name("orders.list").Perm("orders:read")
//	}
//
//	func newAuthorizer() (*casbin.Plugin, error) {
//		cfg := casbin.DefaultConfig()
//		cfg.Policy = "p, user:42, orders:read"
//		return casbin.New(cfg)
//	}
//
// The default resolver obtains user:42 only from web.CurrentPrincipal, which
// must have been populated by verified upstream authentication. A custom
// SubjectResolver has the same trust obligation and must not derive identity
// from an unverified header or bearer token.
//
// Importing this package has no registration side effects. Import
// github.com/xbcio/xbc/transport/web/integrations/casbin/autoload for process-wide
// composition, or use Definition/Bundle with a private assembly.
package casbin
