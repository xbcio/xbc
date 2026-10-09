// Package starter provides XBC's product-level Web baseline: one value that
// selects the Bundles a Web service needs and turns on the capabilities among
// them that are dormant by default.
//
// It is the difference between composing a baseline and having one. The prelude
// contributes the same Definitions as side-effect-free composition data and
// deliberately changes none of their activation policies, so on a process that
// was given no configuration file it delivers an HTTP server and health probes
// and nothing else. This package adds the policy half: the starter's
// configuration layer is merged below every other source and writes
// plugins.<key>.enabled for the members the baseline is not complete without.
//
// # What it turns on
//
// Request IDs, access logging, security headers, compression, and cooperative
// request timeouts. The HTTP server, the panic and error boundaries, the
// authentication middleware, the in-flight admission ceiling, and the health
// capability and its probes are assembled by the server itself or are already
// active as soon as their Bundle is selected, so they need no entry here.
//
// # What it deliberately leaves alone
//
// CORS, authentication and authorization policy, business response envelopes,
// API documentation, profiling, metrics and tracing exporters, and external
// infrastructure stay out: each selects an application contract or requires an
// optional dependency. So do the values a capability's own configuration
// carries. Every capability this package turns on already declares production
// defaults of its own -- the CSP and HSTS spellings, the compression minimum,
// the fifteen-second cooperative deadline -- and those declarations are the
// single source of truth for them. A starter that restated them would be a
// second copy of the same decision, drifting from the first one commit at a
// time, so this one decides only which capabilities are on.
//
// A deployment knob such as web.shutdown.pre_drain_delay is left alone for a
// different reason: its right value depends on how the deployment withdraws an
// instance from service, which the framework cannot know. Read the value the
// application sets, not the framework default, when a rolling update drops
// requests.
//
// # Selecting an engine
//
// A Starter does not select a transport engine. This package lives inside the
// transport/web module, and importing an engine adapter would pull that
// engine's dependencies back into the module's own closure -- the reason
// engines are separate modules in the first place. The composition root
// selects exactly one engine Bundle beside this starter.
//
// # Usage
//
//	xbc.Run(
//		xbc.WithStarter(webstarter.Web()),
//		xbc.WithBundles(ginengine.Bundle(), orders.Bundle()),
//	)
//
// Everything the starter contributes is an ordinary lowest-precedence layer,
// so a configuration file, a profile overlay, an Override, or an environment
// variable can disagree with it -- including by turning a capability back off
// with plugins.<key>.enabled: false. A deployment that prefers to be explicit
// about its whole baseline can skip this package and compose the prelude's
// Bundles itself; doctor reports either choice the same way, naming the source
// each section's values came from.
package starter
