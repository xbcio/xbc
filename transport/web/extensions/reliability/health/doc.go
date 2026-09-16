// Package health serves the HTTP liveness and readiness probes for the
// protocol-neutral extensions/reliability/health capability.
//
// This package owns no checks, no aggregation, no probe policy, and no
// Contributor contract: it renders a Report for one wire format. A dependency
// plugin contributes probes by exporting the neutral
// extensions/reliability/health Contributor, never by importing this package.
// Adding another transport therefore means adding a sibling adapter, not
// changing the capability.
//
// # Usage
//
// Bundle selects the adapter together with the aggregator it renders, so a Web
// service needs one entry:
//
//	app, err := xbc.New(xbc.WithBundles(
//		prelude.Bundle(),
//		ginengine.Bundle(),
//		healthhttp.Bundle(),
//	))
//
// The endpoints are registered as public routes, because an operational probe
// that depends on credentials cannot answer whether the instance is serving.
// Configuration is bound from plugins.health-http:
//
//	plugins:
//	  health:
//	    timeout: 2s
//	  health-http:
//	    liveness_path: /healthz
//	    readiness_path: /readyz
//	    detail_policy: never
//
// A down probe answers 503 with the same JSON body shape as an up probe.
// detail_policy defaults to never, so dependency errors -- which routinely carry
// internal addresses -- stay out of the response; they remain available to
// programmatic callers through the neutral Report. With a nonzero
// web.shutdown.pre_drain_delay, Web leaves its listener up after readiness turns
// down so external probes can observe 503 before HTTP draining starts; at the 0s
// default, drain begins immediately. Bundle and ordinary imports are
// side-effect free; executables using xbc.Run may opt into the leaf autoload
// adapter.
package health
