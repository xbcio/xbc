// Package casbin provides route-aware authorization for XBC's Web transport.
//
// By default a protected route is authorized with its RouteInfo.Perm value and
// the verified subject published by the framework's built-in authentication
// middleware through web.SetPrincipal. Applications can
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
//		r.GET("/orders", func(ctx context.Context, c *web.Ctx) error {
//			c.Status(http.StatusNoContent)
//			return nil
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
// For persistent policy and multi-instance synchronization, compose adapter
// and watcher provider plugins and select their exact instances in Casbin's
// configuration. The adapter provider owns schema migration and its backing
// connection; the watcher factory creates a watcher that Casbin owns until
// shutdown:
//
//	plugins:
//	  casbin-gorm:
//	    writer: {}
//	  casbin-redis:
//	    events: {}
//	  casbin:
//	    adapter:
//	      plugin: casbin-gorm
//	      instance: writer
//	    watcher:
//	      plugin: casbin-redis
//	      instance: events
//
// Provider references are resolved through Definition/Bundle composition, not
// casbin.New. Business authorization code should depend on rbac.Manager, which
// provides validated, backend-neutral RBAC checks and management semantics on
// top of this package's rbac.Backend export. Advanced migration and diagnostic
// code that genuinely needs Casbin-specific APIs may instead depend on
// EnforcerProvider and call Enforcer to obtain the live
// *casbin.SyncedEnforcer; it must not retain the enforcer after lifecycle
// shutdown or publish it as process-global state. An external adapter is
// attached without loading during construction so migrations can run first;
// Start opens and attaches the
// watcher, then performs the initial policy load. External adapters use
// Casbin autosave, while inline/file sources remain read-only with autosave
// disabled.
//
// Importing this package has no registration side effects. Import
// github.com/xbcio/xbc/transport/web/extensions/authorization/casbin/autoload for process-wide
// composition, or use Definition/Bundle with a private assembly.
package casbin
