// Command worker demonstrates a background-only XBC service: an application
// that selects no transport at all and still gets configuration, a lifecycle,
// health, and deterministic shutdown.
//
// Run from the repository root with:
//
//	go run ./examples/worker --config examples/worker/application.yml
//
// Press Ctrl-C to watch reverse-order shutdown; press it twice to watch the
// runtime abandon a graceful stop. Use the doctor subcommand to inspect the
// plan without constructing anything:
//
//	go run ./examples/worker doctor --config examples/worker/application.yml
//
// The composition below is the whole difference from a Web service. Nothing
// here opens a listener, so what keeps the process running is sweeper's
// critical managed task -- see internal/sweeper. An application whose plugins
// neither open traffic nor submit a critical task is refused at startup rather
// than left idling until a signal.
package main

import (
	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/examples/worker/internal/healthlog"
	"github.com/xbcio/xbc/examples/worker/internal/sweeper"
	"github.com/xbcio/xbc/extensions/reliability/health"
)

func main() {
	xbc.Run(xbc.WithBundles(
		health.Bundle(),
		sweeper.Bundle(),
		healthlog.Bundle(),
	))
}
