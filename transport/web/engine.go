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
type Engine interface {
	Handle(method, path string, chain []Handler)
	NoRoute(chain []Handler)
	// NoMethod installs the chain answering a request whose path is registered
	// under other methods only, and is reached solely when
	// Options.HandleMethodNotAllowed is set. RFC 9110 §15.5.6 requires such a
	// response to carry Allow, and the engine is the only party that knows
	// which methods the path has: an adapter must set the Allow header to the
	// other registered methods before running this chain. The chain itself
	// writes the status and body, so an adapter that skips the header produces
	// a well-formed Problem Detail that is still a protocol violation.
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
	TrustedProxies         []string
	ReadTimeout            time.Duration
	ReadHeaderTimeout      time.Duration
	WriteTimeout           time.Duration
	IdleTimeout            time.Duration
	MaxHeaderBytes         int
	MaxMultipartMemory     int64
	HandleMethodNotAllowed bool

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
