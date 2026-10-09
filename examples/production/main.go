// Command production is XBC's production-style example: one HTTP service with
// a public metadata route, authenticated business routes authorized by Casbin,
// an orders table whose writes are transactional with their outbox events, a
// Prometheus scrape endpoint, and the framework baseline -- request ids,
// structured access logs, security headers, compression, cooperative timeouts,
// and health probes.
//
// This composition root does exactly two things: it selects Bundles and a
// Starter, and it calls xbc.Run. Every policy decision lives in configuration,
// which is what lets the same binary run from application.yml on a workstation
// and from environment variables alone in a container.
package main

import (
	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/examples/production/internal/orders"
	"github.com/xbcio/xbc/examples/production/internal/version"
	"github.com/xbcio/xbc/extensions/messaging/outbox"
	gormplugin "github.com/xbcio/xbc/extensions/storage/gorm"
	ginengine "github.com/xbcio/xbc/transport/web/engines/gin"
	"github.com/xbcio/xbc/transport/web/extensions/authentication/apikey"
	"github.com/xbcio/xbc/transport/web/extensions/authentication/jwt"
	casbinhttp "github.com/xbcio/xbc/transport/web/extensions/authorization/casbin"
	"github.com/xbcio/xbc/transport/web/extensions/observability/metrics"
	"github.com/xbcio/xbc/transport/web/extensions/observability/tracing"
	webstarter "github.com/xbcio/xbc/transport/web/starter"
)

// main runs this process. The composition itself lives in options so that the
// package's own tests can execute the exact composition this binary ships,
// rather than a copy of it that could drift.
func main() {
	xbc.Run(options()...)
}

// options is the whole composition root: Bundles, one Starter, nothing else.
func options() []xbc.Option {
	return []xbc.Option{
		// The starter contributes the Web baseline's Bundles and the lowest
		// configuration layer that switches the dormant ones on, so no
		// deployment has to ship a file restating the framework's own defaults.
		// It never selects an engine, because a Starter that imported one would
		// pull that engine's dependencies into the Web runtime module.
		xbc.WithStarter(webstarter.Web()),
		xbc.WithBundles(
			ginengine.Bundle(),

			// Authentication and authorization. Both schemes stay dormant until
			// their plugin section exists, so a deployment that composes them
			// here and configures only one of them simply has one scheme.
			// casbinhttp.Bundle also selects the protocol-neutral policy engine
			// (plugins.casbin) its middleware enforces with.
			jwt.Bundle(),
			apikey.Bundle(),
			casbinhttp.Bundle(),

			// The protocol-neutral integration: a database this service really
			// writes to, and the transactional outbox that shares its
			// transactions. Both activate only once plugins.gorm and
			// plugins.outbox are configured.
			gormplugin.Bundle(),
			outbox.Bundle(),

			// Observability. Metrics is self-contained: enabling
			// plugins.metrics.http publishes a scrape endpoint on this
			// process's own listener, which is why it can be part of a
			// zero-infrastructure start. Tracing is composed but left dormant
			// in application.yml on purpose: its OTLP exporter needs a
			// collector to exist, and a deployment that has none should not be
			// started into a pipeline that fails on every batch.
			metrics.Bundle(),
			tracing.Bundle(),

			// Application plugins.
			version.Bundle(),
			orders.Bundle(),
		),
	}
}
