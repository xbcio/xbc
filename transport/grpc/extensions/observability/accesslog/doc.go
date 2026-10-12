// Package accesslog emits one structured, secret-minimized record for each
// gRPC call the server handles, including calls that unwind through the
// Server's panic boundary.
//
// A record carries the service, the method, the call kind (unary,
// client_stream, server_stream, or bidi_stream), the status code the client
// receives, the latency, the transport peer's IP address, and whether the call
// panicked. Metadata, request and response messages, status messages, error
// text, and recovered panic values are deliberately never recorded: metadata
// carries credentials such as the authorization header, and the rest are
// common paths for payload and PII leaks. The peer is the transport address
// only; proxy metadata such as x-forwarded-for is caller-controlled and is not
// read.
//
// The interceptors run in the observation phase, outside the status boundary,
// so the recorded code is the mapped one on the wire -- a handler's raw error
// is recorded as codes.Internal, not as whatever the error said. A stream is
// one record, written when its handler returns. A panicking call is recorded
// as codes.Internal and the panic continues to the boundary that recovers it.
//
// Records are written at Error level for a panic or a server-fault code
// (Unknown, Internal, Unimplemented, Unavailable, DataLoss, DeadlineExceeded),
// at Warn level when slow_call is set and the call took at least that long,
// and at Info level otherwise.
//
// # Usage
//
// The gRPC prelude includes this Bundle, and the gRPC starter turns it on.
// Composed directly, a plugins.grpc-accesslog section activates it:
//
//	plugins:
//	  grpc-accesslog:
//	    skip_methods: ["/grpc.health.v1.Health/*"]
//	    slow_call: 500ms
//
// A skip method is either an exact full method name or a prefix ending in '*'.
// The section is distinct from the Web access log's plugins.accesslog, so a
// process serving both transports configures each on its own.
//
// Importing this package has no registration side effects. Applications that
// use XBC's process-wide catalog opt in with a blank import of
// github.com/xbcio/xbc/transport/grpc/extensions/observability/accesslog/autoload.
package accesslog
