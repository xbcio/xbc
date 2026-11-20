// Package management serves the gRPC stack's operator endpoints on a second,
// optional listener: the neutral metrics plugin's exposition at /metrics when
// that plugin is selected, and Go's standard profiling handlers under
// /debug/pprof/ when pprof is enabled.
//
// The listener exists because a scrape or a profile is not business traffic.
// It must be reachable while the serving listener is busy draining, and it
// must not be reachable from wherever the business port is exposed. Both facts
// follow from the same rule: the plugin opens no traffic and depends on none,
// so it is bound in Start but never admitted before the runtime's traffic gate
// releases, and the shutdown walk stops it after every ingress plugin. A
// scrape issued while business traffic is still finishing therefore lands,
// and a scrape issued once the serving listener is gone is refused rather
// than answered by a process about to exit. This is the same reachability
// window and stop ordering the Web transport's management plane guarantees;
// the two implementations share the policy, not code -- this package never
// imports transport/web, and the two listeners of a dual-transport process
// may both be enabled, each exposing the same registry.
//
// Access control on the listener is the bound address, not an authentication
// policy: an operator surface with no business identity has no principal to
// authenticate. A hostless address binds loopback, a non-loopback host is
// refused without allow_remote, and allow_remote is refused without TLS, so
// the listener is never published unencrypted. There is deliberately no
// health route: readiness is consulted by whatever routes traffic to this
// process, which is the serving listener's job.
//
// # Usage
//
// The gRPC prelude does not include this Bundle: an operator surface is a
// deployment decision. Select it explicitly, beside the neutral metrics plugin
// whose registry it exposes:
//
//	plugins:
//	  metrics: {}
//	  grpc-management:
//	    addr: "127.0.0.1:9091"
//	    pprof: false
//	    tls:
//	      cert_file: /etc/xbc/management.crt
//	      key_file: /etc/xbc/management.key
//
// With metrics selected, /metrics renders the same registry the serving
// process records into, including the built-in gRPC instrumentation. Without
// it -- or with pprof off -- the corresponding route is not registered and
// answers 404, so an absent endpoint is never mistaken for an empty one.
//
// Importing this package has no registration side effects. Applications that
// want the management plane without naming it in code import the autoload
// package; everyone else composes Bundle() at the root.
package management
