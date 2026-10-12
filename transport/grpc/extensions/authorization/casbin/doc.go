// Package casbin provides method-aware authorization for XBC's gRPC
// transport.
//
// This package is the gRPC half of a pair. The policy engine -- model, policy
// sources, adapter and watcher providers, periodic reload, and the
// rbac.Backend and EnforcerProvider contracts -- lives in the protocol-neutral
// github.com/xbcio/xbc/extensions/authorization/casbin, whose plugin key is
// "casbin". This package is an interceptor plugin keyed "casbin-grpc" that
// consumes that engine's EnforcerProvider and owns no enforcer itself: a
// non-gRPC service composes the engine without any gRPC surface, and this
// interceptor's Stop fails methods closed without stopping the engine other
// consumers may still be using.
//
// A protected method is authorized with its registration-time permission,
// declared by the registering plugin through the Methods handle, and the
// verified subject the framework's authentication stage published. The
// interceptor implements authentication.RequiresPrincipal, so the framework
// pins it after that stage on both the unary and the stream chain, and a
// stream is authorized once, when it is established.
//
// # Usage
//
// Compose the interceptor bundle, which includes the engine, and configure
// both products. Mark protected methods with permissions and grant those
// permissions in the Casbin policy. The secure default denies a protected
// method whose permission is missing:
//
//	func (p *Orders) RegisterServices(r *xbcgrpc.Registrar) error {
//		orderspb.RegisterOrdersServer(r, p.orders)
//		r.Methods("orders.Orders").Perm("orders:read").MethodPerm("Place", "orders:write")
//		return nil
//	}
//
//	plugins:
//	  casbin:
//	    policy: "p, user:42, orders:read"
//	  casbin-grpc: {}
//
// Applications can instead use the engine's path_method convention, whose
// request tuple is (subject, "/pkg.Service/Method", "RPC"); the interceptor
// derives once, at construction, which tuple to evaluate from the model the
// engine built. A method the authentication stage resolved as permitted is
// skipped, exactly like an exempt Web route; a call that reached this
// interceptor without a published Principal is denied.
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
//	  casbin-grpc: {}
//
// Provider references are resolved through Definition/Bundle composition, not
// through a constructor. The engine's package documentation covers its
// configuration, its rbac.Backend export, and the enforcer's advanced
// EnforcerProvider API. Importing this package has no registration side
// effects. Import
// github.com/xbcio/xbc/transport/grpc/extensions/authorization/casbin/autoload
// for process-wide composition, or use Definition/Bundle with a private
// assembly.
package casbin
