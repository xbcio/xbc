package websocket

import (
	"context"
	"net/http"

	coderwebsocket "github.com/coder/websocket"

	"github.com/xbcio/xbc/transport/web"
)

// Options configures one accepted connection. The zero value accepts same-host
// origins only, negotiates no subprotocol, and disables compression.
type Options struct {
	// OriginPatterns lists extra hosts, as path.Match patterns, that may open
	// a cross-origin connection. The request's own host is always allowed. A
	// pattern that matches every host -- "*", "**", or "*://*" -- disables the
	// origin check for this route and must never be used; see the package
	// documentation.
	OriginPatterns []string

	// Subprotocols lists the subprotocols the server may negotiate. Empty
	// negotiates none.
	Subprotocols []string

	// Compression enables per-message deflate with no context takeover.
	Compression bool

	// ReadLimit bounds one received message in bytes. Zero keeps the protocol
	// library's own default.
	ReadLimit int64

	// CompressionThreshold is the minimum message size compressed when
	// Compression is enabled. Zero keeps the protocol library's own default.
	CompressionThreshold int
}

// Upgrader is the contract an application plugin depends on to register
// WebSocket endpoints. The composed Plugin implements it; constructing a
// separate implementation would own a registry nobody drains.
type Upgrader interface {
	// Handler returns a web.Handler that upgrades the request and then runs
	// serve on the accepted connection. serve returns when the conversation
	// ends; its error is logged and never rendered because the 101 response
	// is already committed.
	Handler(opts Options, serve func(context.Context, *Conn) error) web.Handler
}

// Conn is the accepted connection. It is github.com/coder/websocket.Conn,
// re-exported so application code depends on this module rather than importing
// the third-party package directly.
type Conn = coderwebsocket.Conn

// Handler returns a web.Handler that upgrades the request and then runs serve.
//
// The handler holds one admission slot for the life of the connection: it
// blocks until serve returns, so a route serving sockets needs
// web.max_in_flight sized for the expected number of concurrent connections
// and must not be marked Unmetered.
func (p *Plugin) Handler(opts Options, serve func(context.Context, *Conn) error) web.Handler {
	acceptOptions := &coderwebsocket.AcceptOptions{
		// Copy the slices so a caller mutating them after this call cannot
		// change the policy of an already-registered route.
		OriginPatterns:       append([]string(nil), opts.OriginPatterns...),
		Subprotocols:         append([]string(nil), opts.Subprotocols...),
		CompressionThreshold: opts.CompressionThreshold,
	}
	if opts.Compression {
		acceptOptions.CompressionMode = coderwebsocket.CompressionNoContextTakeover
	}
	readLimit := opts.ReadLimit
	return func(ctx context.Context, c *web.Ctx) error {
		if c == nil || c.Request() == nil || c.Writer() == nil {
			return nil
		}
		if p.isDraining() {
			// Refuse before any protocol bytes are written, so the client
			// receives a normal HTTP Problem Detail rather than a failed
			// handshake.
			web.AbortProblem(c, web.NewProblem(http.StatusServiceUnavailable, problemCodeServiceUnavailable))
			return nil
		}
		conn, err := coderwebsocket.Accept(upgradeWriter{writer: c.Writer()}, c.Request(), acceptOptions)
		if err != nil {
			// Accept writes its own HTTP response on every failure path, so the
			// request is already answered. Web's error resolver does not render
			// a written response; it logs it. Returning the error would log the
			// same failure twice -- once here with the upgrade error and once
			// through the resolver -- so it is logged once and nil is returned.
			p.log().Error("websocket: upgrade failed", "error", err)
			return nil
		}
		if readLimit > 0 {
			conn.SetReadLimit(readLimit)
		}
		entry, admitted := p.register(conn)
		if !admitted {
			// Drain or Stop set the flag after the pre-upgrade check but
			// before this handshake completed. Nothing would wait for this
			// connection if it were served, so close it now.
			_ = conn.CloseNow()
			return nil
		}
		defer func() {
			// Close the socket before publishing completion. Drain and Stop
			// wait on done, so unregistering first would let them report a
			// finished shutdown while this hijacked connection is still open:
			// CloseNow can block behind Drain's in-progress close handshake,
			// and the caller's context bounds that wait, not this call.
			_ = conn.CloseNow()
			p.unregister(entry)
		}()
		if serve == nil {
			p.log().Error("websocket: Handler requires a non-nil serve function")
			return nil
		}
		if err := serve(ctx, conn); err != nil {
			// The 101 response is already committed, so there is nothing to
			// render. Log it here and return nil rather than handing it to
			// Web's resolver, which would only log the same failure again.
			p.log().Error("websocket: connection handler failed", "error", err)
		}
		return nil
	}
}

// upgradeWriter adapts the neutral writer for the protocol library's upgrade.
//
// The library writes the 101 response through the writer it is given and then
// hijacks the connection. A neutral writer may be two-phase: WriteHeader
// records the status and a later commit sends it. The library therefore calls
// the optional WriteHeaderNow hook before hijacking, and this adapter maps that
// hook to Flush, the neutral contract's commit operation. Without the adapter
// an engine whose writer records the status -- the neutral test engine's
// recorder is one -- would never send the 101, and the client would wait for a
// handshake response that was never written.
//
// Unwrap exposes the neutral writer so the library's hijack walk reaches the
// engine's own http.Hijacker through the rest of the chain.
type upgradeWriter struct {
	writer web.ResponseWriter
}

func (w upgradeWriter) Header() http.Header { return w.writer.Header() }

func (w upgradeWriter) WriteHeader(code int) { w.writer.WriteHeader(code) }

func (w upgradeWriter) Write(data []byte) (int, error) { return w.writer.Write(data) }

func (w upgradeWriter) WriteHeaderNow() { w.writer.Flush() }

func (w upgradeWriter) Unwrap() http.ResponseWriter { return w.writer }

var _ http.ResponseWriter = upgradeWriter{}
