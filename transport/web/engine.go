package web

import (
	"context"
	"net"
	"time"
)

// Engine is the port every HTTP engine adapter implements. The chain is
// flattened by the router before registration, so the port deliberately has
// no Use or Group: middleware accumulation is xbc's job, not the engine's.
type Engine interface {
	Handle(method, path string, chain []Handler)
	NoRoute(chain []Handler)
	NoMethod(chain []Handler)
	Serve(ln net.Listener) error
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
}

// EngineFactory is exported by an engine plugin. web.Server calls it during
// Start with neutral options; the engine never reads the web.* section.
type EngineFactory interface {
	NewEngine(Options) (Engine, error)
}
