// Package health serves the standard grpc.health.v1.Health service for the
// protocol-neutral extensions/reliability/health capability.
//
// This package owns no checks, no aggregation, and no probe policy: it renders
// a Report for one wire format. A dependency plugin contributes probes by
// exporting the neutral extensions/reliability/health Contributor, never by
// importing this package. Adding another transport therefore means adding a
// sibling adapter, not changing the capability.
//
// # Usage
//
// Bundle selects the adapter together with the aggregator it renders, so a gRPC
// service needs one entry:
//
//	app, err := xbc.New(xbc.WithBundles(
//		grpc.Bundle(),
//		health.Bundle(),
//	))
//
// The service is registered as public, because an operational probe that
// depends on credentials cannot answer whether the instance is serving; a
// grpc.security rule can still tighten it. It reads plugins.health:
//
//	plugins:
//	  health:
//	    timeout: 2s
//
// Check answers SERVING or NOT_SERVING from the aggregator's readiness probe:
// the empty service name (what load balancers and grpc_health_probe dial by
// default) and the explicit "readiness" name both mean readiness, "liveness"
// means liveness, and any other name answers NOT_FOUND rather than guessing.
// Until the runtime releases the traffic gate the answer is NOT_SERVING, and
// during shutdown readiness turns down as soon as the execution context is
// cancelled -- while the server is still draining in-flight RPCs -- so a load
// balancer sees the instance leave rotation before it stops accepting.
//
// Watch and List answer UNIMPLEMENTED. The neutral aggregator is a pull-only
// contract with no change notification, so a pushed status would be either
// stale or a polling loop inventing its own timing; Check is the probe this
// adapter promises, and every standard gRPC health checker uses it.
//
// Bundle and ordinary imports are side-effect free; executables using xbc.Run
// may opt into the leaf autoload adapter.
package health
