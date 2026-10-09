package web

import (
	"context"
	"net"
	"time"

	"github.com/xbcio/xbc/log"
)

// Engine is the port every HTTP engine adapter implements. The chain is
// flattened by the router before registration, so the port deliberately has
// no Use or Group: middleware accumulation is xbc's job, not the engine's.
//
// An adapter must not answer a request itself. Every response has to come from
// one of the three chains this port installs -- the matched route chain,
// NoRoute, or NoMethod -- because everything xbc promises about a response is
// a stage of that chain: the in-flight gate, the panic and error boundaries,
// contributed middleware, request id, access log, CORS, security headers and
// business metrics. A response an engine's own matcher produces bypasses all
// of it. Engine conveniences that would otherwise reply first -- trailing-slash
// redirects, fixed-path rewrites, automatic OPTIONS answers -- must therefore
// be disabled by the adapter, not merely left unconfigured: an engine default
// that answers is the same bypass with a different author, and an unmatched
// path has to reach NoRoute so it can render the framework's 404 Problem
// Detail like any other.
type Engine interface {
	Handle(method, path string, chain []Handler)
	// Mount installs chain for a whole subtree on one method: the literal
	// prefix itself and every path beneath it, never a path that merely
	// shares its characters ("/flow" owns "/flow" and "/flow/...", not
	// "/flowx"). It is a separate port method because a subtree's syntax is
	// the engine's own -- gin spells it with a catch-all segment, ServeMux
	// with a trailing slash -- so a caller that wrote either one into a path
	// handed to Handle would be writing one engine's dialect into the route
	// table. The caller asks for "the prefix and everything under it" and the
	// adapter translates.
	//
	// The boundaries Handle draws are unchanged: the adapter splices nothing
	// into the chain it is handed, and the chain -- never the engine's own
	// matcher -- answers the request. Inside the subtree that means a path
	// the chain's terminal handler does not serve is a 404 that handler
	// writes, and a method the caller did not mount reaches NoMethod with
	// Allow, exactly as for an ordinary route.
	//
	// Mount is per-method; a subtree served under several methods is several
	// calls. Registrations never overlap -- the caller refuses a mount at or
	// under an existing registration on the same method before either adapter
	// sees it -- so an adapter receives prefixes that are disjoint at path
	// segment boundaries.
	Mount(method, prefix string, chain []Handler)
	NoRoute(chain []Handler)
	// NoMethod installs the chain answering a request whose path is registered
	// under other methods only. Distinguishing that case from an unmatched path
	// is required, not optional: Web's HTTP error contract names 405 as one of
	// its Problem Details, and an engine that folded it into 404 would silently
	// drop that guarantee. RFC 9110 §15.5.6 requires such a response to carry
	// Allow, and the engine is the only party that knows which methods the path
	// has: an adapter must set the Allow header to the other registered methods
	// before running this chain. The chain itself writes the status and body, so
	// an adapter that skips the header produces a well-formed Problem Detail
	// that is still a protocol violation.
	NoMethod(chain []Handler)
	Serve(ln net.Listener) error
	// Shutdown stops serving and must leave no established connection behind
	// once ctx is done. Draining in-flight requests first is expected, but an
	// implementation that cannot finish draining within the deadline is
	// required to force the remaining connections closed before returning;
	// reporting the drain failure while letting those connections outlive the
	// deadline is not an acceptable implementation.
	Shutdown(ctx context.Context) error
}

// Options carries the neutral engine settings owned by the web.* config
// section. Engine-specific knobs belong to the engine plugin's own section
// and never travel through here.
type Options struct {
	TrustedProxies     []string
	ReadTimeout        time.Duration
	ReadHeaderTimeout  time.Duration
	WriteTimeout       time.Duration
	IdleTimeout        time.Duration
	MaxHeaderBytes     int
	MaxMultipartMemory int64

	// Logger is the Server's logger, handed to the engine so an adapter can
	// route the engine's own diagnostic output into xbc's logging instead of
	// the process's standard streams, and can match its verbosity to what that
	// logger accepts. An engine that produces no such output ignores it.
	//
	// Server always supplies one. An application constructing Options by hand
	// may leave it nil, and an adapter must tolerate that.
	Logger log.Logger
}

// EngineFactory is exported by an engine plugin. web.Server calls it during
// Start with neutral options; the engine never reads the web.* section.
type EngineFactory interface {
	NewEngine(Options) (Engine, error)
}
