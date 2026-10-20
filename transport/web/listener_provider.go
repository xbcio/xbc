package web

import "net"

// ListenerProvider supplies the listener a Web server plane serves through. It
// replaces the net.Listen("tcp", address) call Start would otherwise make, so a
// deployment can serve from a Unix domain socket, a descriptor inherited from
// systemd socket activation, or a wrapper that consumes a PROXY-protocol header
// before the TLS records begin.
//
// It is an optional plugin export: a composition that exports none keeps
// exactly today's TCP behavior, and a composition that exports more than one
// fails assembly with the framework's standard ambiguity error rather than
// letting boot order decide. The provider receives both configured planes; see
// the call rules below.
//
// # Call rules
//
// Start calls Listen at most once per configured plane: the serving plane first,
// with the configured web.addr, then the management plane with the resolved
// management address when one is configured. No other path calls it. Preflight
// -- and therefore the validate command -- binds nothing and calls no provider,
// and Stop neither binds nor accepts a listener back.
//
// The address is the one the plane would otherwise pass to
// net.Listen("tcp", address). A provider may use it, reinterpret it, or ignore
// it, which is what socket activation does. web.addr stays required and
// non-empty because configuration is validated before plugin inputs exist; its
// host:port shape is not validated, and the TCP path reports a malformed
// address at bind time. With a provider it is advisory, so a provider that
// binds something other than TCP keeps a plausible placeholder (the default
// ":8080" is one) and reads its real address from its own configuration
// section.
//
// # Ownership
//
// The returned listener must be unstarted. Ownership transfers to the Server
// for the life of that plane: the Server hands the listener to Engine.Serve,
// closes it during Stop or on a later start failure, and explains the shutdown
// in its own documentation. The provider must not close it and must not serve
// from it.
//
// The Server wraps the returned listener with the configured TLS policy exactly
// as it wraps a TCP listener, so TLS always sits above the provider. A
// PROXY-protocol provider must therefore consume the header before TLS records:
// its Accept must return a connection whose stream begins after the header.
//
// # Error rules
//
// An error aborts Start and names the provider identity and the address:
//
//	xbc: listener provider <identity> failed to listen on <address>: <cause>
//
// If Listen returns both a listener and an error, the listener is closed before
// Start returns. A nil listener with a nil error is refused with its own
// message naming the provider and address, rather than surfacing later as a nil
// dereference. The same refusal covers a typed-nil listener -- a non-nil
// interface holding a nil pointer -- because that value is equally unusable and
// would otherwise panic inside the serving goroutine after Start reported
// success. A panic from the provider propagates: it is a startup programming
// error, like a route registration panic.
type ListenerProvider interface {
	// Listen returns an unstarted listener for address. The address is the one
	// the plane would otherwise pass to net.Listen("tcp", address); a provider
	// may use it, reinterpret it, or ignore it (socket activation).
	Listen(address string) (net.Listener, error)
}
