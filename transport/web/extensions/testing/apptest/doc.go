// Package apptest starts a composed XBC application inside the test process
// and exposes it over real HTTP.
//
// # Usage
//
// Compose the application under test exactly as its binary would, then hand
// the additional Bundles to New:
//
//	app := apptest.New(t,
//		apptest.WithBundles(greeter.Bundle()),
//		apptest.WithYAML("greeter:\n  prefix: hi\n"),
//	)
//
//	response, err := app.Client.Get(app.URL("/hello"))
//
// New runs the real runtime -- configuration, planning, construction,
// lifecycle, and the Web server -- rather than a hand-wired HTTP handler, so
// every assertion observes the wiring the process would have. The Web server,
// the Gin engine adapter, and this package's probe plugin are always composed;
// everything else comes from WithBundles.
//
// # Configuration
//
// The generated file contains only what the harness owns:
//
//	web:
//	  addr: 127.0.0.1:<port>
//	xbc:
//	  shutdown_timeout: 5s
//
// The port is drawn by reserving and closing a loopback listener, because the
// Web plugin does not export the address it binds. A boot that loses the port
// to another process before it binds is retried on a fresh one.
//
// WithYAML supplies an overlay merged above that file. It is written as the
// profile sibling the loader reads after the base file and passed as
// --profile, rather than appended to the base file: appending a second "web:"
// or "xbc:" document would be a duplicate top-level key, which the YAML parser
// rejects.
package apptest
