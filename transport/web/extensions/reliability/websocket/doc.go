// Package websocket accepts WebSocket connections through the Web transport.
//
// The neutral web.ResponseWriter deliberately exposes no Hijack, so an
// application cannot upgrade a connection through it directly. This package
// owns the upgrade, the accepted connection, and the shutdown of connections
// that http.Server.Shutdown can no longer see: once a connection is hijacked,
// the HTTP server no longer tracks it.
//
// # Usage
//
// Compose Bundle at the composition root, resolve the Upgrader it exports as
// a plugin input, and register Handler on an ordinary route:
//
//	var upgraderInput = plugin.RequireOne[websocket.Upgrader]()
//
//	type Chat struct {
//		upgrader websocket.Upgrader
//	}
//
//	func newChat(ctx plugin.BuildContext) (*Chat, error) {
//		return &Chat{upgrader: upgraderInput.Get(ctx).Value}, nil
//	}
//
//	func (h *Chat) RegisterRoutes(router *web.Router) {
//		router.GET("/chat", h.upgrader.Handler(websocket.Options{}, h.serve)).
//			Name("chat").
//			Auth(web.Public())
//	}
//
//	func (h *Chat) serve(ctx context.Context, conn *websocket.Conn) error {
//		typ, message, err := conn.Read(ctx)
//		if err != nil {
//			return nil
//		}
//		return conn.Write(ctx, typ, message)
//	}
//
// Handler is a web.Handler, so the route's authentication, permission, and
// admission policy run before the upgrade. Resolve the Upgrader from the graph
// rather than constructing a separate New instance: the lifecycle drains the
// registry the composed instance owns, and a second registry would never be
// drained.
//
// # Origin policy
//
// The zero Options value accepts the request's own host only, negotiates no
// subprotocol, and disables compression. OriginPatterns adds extra hosts as
// path.Match patterns for a cross-origin deployment; the request host is
// always allowed. The package exports no switch that disables the check, but a
// pattern that matches every host -- "*", "**", or "*://*" -- is equivalent to
// one and must never be used: it would turn every WebSocket endpoint into a
// cross-site request forgery target. Such a pattern is not rejected at
// registration: "matches every host" cannot be decided for path.Match, and a
// partial check would give false assurance, so the prohibition is stated here
// and on Options.OriginPatterns instead.
//
// # Admission
//
// An accepted socket keeps its HTTP handler running for the life of the
// conversation, so it holds one web.max_in_flight slot the whole time. Size
// that ceiling for the expected number of concurrent sockets. Do not mark the
// route Unmetered: the exemption bypasses admission entirely and would let
// unlimited sockets past the gate.
//
// # Lifecycle
//
// The plugin implements plugin.Drainer and plugin.Closer. Drain stops
// admitting upgrades -- an upgrade attempted during the drain is answered with
// the 503 service_unavailable Problem Detail before any protocol bytes are
// written -- starts a going-away close handshake on every registered
// connection, and waits for the handlers to return or for its context to
// expire. An expired context stops the wait; the sockets stay open for Stop.
// Stop force-closes every remaining connection, one goroutine per connection,
// and waits for the handlers bounded by its context: an expired context stops
// the wait, not the force-close, so a socket stuck in a close handshake cannot
// hold the stop phase past its budget. Stop is correct whether Drain ran,
// timed out, failed, or never ran. Both are safe before Start, after a failed
// Start, and more than once.
//
// In the repository's shutdown terms, Drain ends the conversation and leaves
// the transport to Stop: it never force-closes a connection and never releases
// anything an in-flight handler still uses.
//
// A serve function returns when the connection ends and decides for itself
// whether the conversation ended normally. Its error is logged, never
// rendered: after the 101 response the HTTP response is already committed, so
// nothing can add a Problem Detail to it. An upgrade failure is different:
// the protocol library writes its own HTTP error response, and Web's error
// resolver logs rather than renders a response that is already written.
// Returning Accept's error would therefore not append a Problem Detail -- it
// would log the same failure a second time -- so the handler logs it once with
// the upgrade error and returns nil.
package websocket
