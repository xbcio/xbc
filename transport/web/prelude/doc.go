// Package prelude provides XBC's lightweight, production-safe Web baseline as
// side-effect-free composition data. It is a pure aggregate: it declares no
// Definition and creates no runtime unit of its own.
//
// Its Bundle explicitly combines member Bundles for the HTTP server, recovery,
// request IDs, structured access logging, security headers, compression,
// cooperative request timeouts, and health probes. Every Definition contributed
// by those members remains independent: web.Bundle(), for example, contributes
// both the primary HTTP server Definition and its independently ordered error
// boundary. Prelude preserves each member Definition's activation policy and
// configuration path; it neither clones declarations nor registers from init.
//
// Business response envelopes, CORS, authentication, authorization,
// persistence, telemetry exporters, and API documentation are deliberately not
// included because they select an application contract or policy, or require
// an optional third-party dependency.
//
// Prelude is not, by itself, a startable baseline: web.Bundle()'s server
// Definition requires exactly one web.EngineFactory input, and prelude
// contributes none. Prelude cannot make that choice itself -- it lives inside
// the transport/web module, and importing any engine adapter (for example
// transport/web/engines/gin) would pull that engine's own dependencies back
// into transport/web's dependency closure, defeating the reason engines are
// kept as separate modules. A composition root must therefore also select
// exactly one engine Bundle alongside prelude.Bundle().
//
// # Usage
//
// Explicit composition is the primary API:
//
//	app, err := xbc.New(xbc.WithBundles(
//		prelude.Bundle(),
//		ginengine.Bundle(),
//		orders.Bundle(),
//	))
//
// Executables that deliberately prefer xbc.Run may blank import the optional
// transport/web/autoload leaf adapter, but autoload.Declare(prelude.Bundle())
// alone cannot assemble a startable app either, for the same reason: the
// composition root must still separately select an engine Bundle. Importing
// prelude itself never mutates process-global state.
package prelude
