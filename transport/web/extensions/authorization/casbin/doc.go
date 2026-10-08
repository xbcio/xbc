// Package casbin provides route-aware authorization for XBC's Web transport.
//
// This package is the Web half of a pair. The policy engine -- model, policy
// sources, adapter and watcher providers, periodic reload, and the
// rbac.Backend and EnforcerProvider contracts -- lives in the protocol-neutral
// github.com/xbcio/xbc/extensions/authorization/casbin, whose plugin key is
// "casbin". This package is a middleware plugin keyed "casbin-http" that
// consumes that engine's EnforcerProvider and owns no enforcer itself: a
// non-Web service composes the engine without any HTTP surface, and this
// middleware's Stop fails matched routes closed without stopping the engine
// other consumers may still be using.
//
// By default a protected route is authorized with its RouteInfo.Perm value and
// the verified subject published by the framework's built-in authentication
// middleware through web.SetPrincipal. Applications can instead use the
// engine's path_method convention -- the middleware derives once, at
// construction, which request tuple to evaluate from the model the engine
// built -- or inject a SubjectResolver for a different, already-verified
// identity source. This package never parses credentials or unverified bearer
// tokens itself.
//
// # Usage
//
// Compose the middleware bundle, which includes the engine, and configure both
// products. Mark protected routes with permissions and grant those permissions
// in the Casbin policy. The secure default denies a protected route whose
// permission is missing:
//
//	func (*Orders) RegisterRoutes(r *web.Router) {
//		r.GET("/orders", func(ctx context.Context, c *web.Ctx) error {
//			c.Status(http.StatusNoContent)
//			return nil
//		}).Name("orders.list").Perm("orders:read")
//	}
//
//	plugins:
//	  casbin:
//	    policy: "p, user:42, orders:read"
//	  casbin-http: {}
//
// The default resolver obtains user:42 only from web.CurrentPrincipal, which
// must have been populated by verified upstream authentication. A custom
// SubjectResolver has the same trust obligation and must not derive identity
// from an unverified header or bearer token.
//
// For persistent policy and multi-instance synchronization, compose adapter
// and watcher provider plugins from the neutral module and select their exact
// instances in the engine's configuration:
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
//	  casbin-http: {}
//
// Provider references are resolved through Definition/Bundle composition, not
// through a constructor. The engine's package documentation covers its
// configuration, its rbac.Backend export, and the enforcer's advanced
// EnforcerProvider API. Importing this package has no registration side
// effects. Import
// github.com/xbcio/xbc/transport/web/extensions/authorization/casbin/autoload
// for process-wide composition, or use Definition/Bundle with a private
// assembly.
package casbin
