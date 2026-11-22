// Package elasticx provides named timeout-aware Elasticsearch clients with
// bounded asynchronous bulk indexing. This module pins
// github.com/elastic/go-elasticsearch/v8 at v8.19.1. Importing this package is
// side-effect free. Prefer explicit composition with elasticx.Bundle();
// executables that intentionally use process-wide autoload may blank-import
// the autoload subpackage.
//
// # Usage
//
// Each named section below plugins.elasticsearch constructs one primary
// *elasticx.Client. Its concrete type and BulkIndexer contract resolve to
// the same plugin identity and lifecycle owner. A consumer declares exact typed
// inputs and reads them only during construction:
//
//	var searchClient = plugin.RefToInstance[*elasticx.Client](
//		elasticx.Key,
//		"search",
//	)
//	var searchBulk = plugin.RefToInstance[elasticx.BulkIndexer](
//		elasticx.Key,
//		"search",
//	)
//
//	type searchIndex struct {
//		client *elasticx.Client
//		bulk   elasticx.BulkIndexer
//	}
//
//	var searchDefinition = plugin.Define(
//		"search-index",
//		func(ctx plugin.BuildContext) (*searchIndex, error) {
//			return &searchIndex{
//				client: searchClient.Get(ctx).Value,
//				bulk:   searchBulk.Get(ctx).Value,
//			}, nil
//		},
//		plugin.Options[*searchIndex]{
//			Inputs: plugin.Inputs(searchClient, searchBulk),
//		},
//	)
//
//	func Bundle() plugin.Bundle {
//		return plugin.BundleOf(searchDefinition)
//	}
//
// The composition root includes both elasticx.Bundle() and the consumer's
// Bundle. Client.Do applies the configured timeout, and callers must close every
// successful response body. Client.Raw is an explicit escape hatch to the
// vendor's untyped generated API, and Client.Typed is the same escape hatch to
// its typed API; neither applies the timeout Do applies. Both are built during
// construction from one configuration -- addresses, authentication, TLS,
// compression, logging, and transport -- so the two cannot drift apart. Bulk
// admission is bounded and uses the configured block or reject policy; Flush is
// a barrier for previously admitted items. A Client built by New starts its
// bulk worker on the first Add or Flush, so a client used only for synchronous
// requests owns no worker goroutine; a Definition-based client starts its
// worker at Start. Drain stops bulk admission and waits for every admitted item
// to be flushed, leaving the transport open; Stop finishes that flush if Drain
// did not, then closes the transport, including the typed client's.
//
// # Transport and TLS
//
// tls.ca_file supplies the PEM bundle HTTPS nodes are verified against. TLS
// verification is on by default; tls.insecure_skip_verify is the explicit
// opt-in for an internal cluster with a self-signed certificate, and enabling
// it builds a dedicated transport from http.DefaultTransport rather than
// silently weakening the default path. compress_request_body gzips request
// bodies and is off unless a deployment asks for it.
//
// The package-built transport otherwise follows http.DefaultTransport,
// including its proxy environment variables (HTTP_PROXY, HTTPS_PROXY,
// NO_PROXY). Set disable_proxy for an internal cluster that must be reached
// directly. A program that needs full control can instead take ownership of the
// transport with New's WithTransport option; TLS and proxy configuration are
// then rejected instead of silently ignored, because the caller's
// http.RoundTripper owns those behaviors.
//
// Supply API keys, passwords, and private certificate authorities through
// secret-backed configuration. Credentials in node URLs are rejected, and this
// package does not include configured credentials in its own errors or logs.
// Username and password must be configured together: a password-only section
// cannot authenticate, and a username-only one is usually a secret-injection
// failure. The one explicit exception is allow_empty_password, which accepts a
// username without a password for clusters that need no authentication but
// whose shared configuration still carries a service username. Note that
// go-elasticsearch sends Basic authentication only when both fields are
// non-empty, so that exception accepts the username without using it as a
// credential; requests stay unauthenticated.
//
// # Logging
//
// Logging is off by default and is configured only in code, through New's
// options. WithTransportLogger installs go-elasticsearch's request/response
// logger so a program can route round-trip records into its own logging
// system. WithDebugLogger enables the vendor's connection-management debug
// logger, which writes to stdout and lives in a process-wide vendor global.
// Configuration files carry neither setting, and Definition-based construction
// does not accept code options.
//
// # Readiness
//
// The primary *Client exports health.Contributor and contributes its cluster's
// reachability as a readiness check. No separate Definition is needed: the
// primary is this package's own type, so it carries the contract itself and each
// configured instance appears in the aggregate report under its own identity --
// "elasticsearch" for the default instance and "elasticsearch[<instance>]" for a
// named one. The contribution is inert unless the application also selects the
// health capability Bundle.
//
// An instance configured with health_probe: false contributes nothing. That flag
// already suppresses the Init-time probe, and giving it a second, narrower
// meaning for readiness would make it unpredictable.
//
// probe_policy decides what a failed Init-time probe means. The default "fail"
// aborts construction, which surfaces a wrong address or credential before the
// process serves traffic. "warn" logs the failure and lets the instance start,
// for a cluster whose unavailability must not stop the process; readiness then
// keeps reporting the outage. The policy affects only the startup probe and has
// no effect when health_probe is false.
package elasticx
