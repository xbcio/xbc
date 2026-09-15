package gin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	ginlib "github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

// Factory constructs the gin-backed Engine. It is the only exported
// constructor: web.Server calls Factory{}.NewEngine during Start, and no
// other package needs to reach the concrete *engine type.
type Factory struct{}

type engine struct {
	e   *ginlib.Engine
	srv *http.Server
}

// NewEngine builds a gin engine from neutral Options. It absorbs every
// gin-native registration (*ginlib.Engine, SetTrustedProxies,
// HandleMethodNotAllowed, MaxMultipartMemory) and the *http.Server this
// engine serves through, both previously constructed directly in
// (*web.Server).Start.
func (f Factory) NewEngine(opts web.Options) (web.Engine, error) {
	e := ginlib.New()
	if err := e.SetTrustedProxies(opts.TrustedProxies); err != nil {
		return nil, fmt.Errorf("xbc: web trusted_proxies: %w", err)
	}
	e.HandleMethodNotAllowed = opts.HandleMethodNotAllowed
	e.MaxMultipartMemory = opts.MaxMultipartMemory
	// ContextWithFallback is no longer needed: the neutral Ctx has a single
	// Set/Get store and a single context.Context (the Handler's first
	// parameter), so gin's two disconnected stores never both apply.
	return &engine{
		e: e,
		srv: &http.Server{
			Handler:           e,
			ReadTimeout:       opts.ReadTimeout,
			ReadHeaderTimeout: opts.ReadHeaderTimeout,
			WriteTimeout:      opts.WriteTimeout,
			IdleTimeout:       opts.IdleTimeout,
			MaxHeaderBytes:    opts.MaxHeaderBytes,
		},
	}, nil
}

func (g *engine) Handle(method, path string, chain []web.Handler) {
	g.e.Handle(method, path, g.toGinChain(chain)...)
}

func (g *engine) NoRoute(chain []web.Handler)  { g.e.NoRoute(g.toGinChain(chain)...) }
func (g *engine) NoMethod(chain []web.Handler) { g.e.NoMethod(g.toGinChain(chain)...) }
func (g *engine) Serve(ln net.Listener) error  { return g.srv.Serve(ln) }

// Shutdown drains in-flight requests until ctx is done, then force-closes
// whatever refused to drain. The forced close is what makes the web.Engine
// contract hold: returning the drain error on its own would leave established
// connections serving past the deadline the caller asked to stop within. The
// drain error is still reported, so a caller can tell a clean drain from a
// deadline it had to be rescued from.
func (g *engine) Shutdown(ctx context.Context) error {
	err := g.srv.Shutdown(ctx)
	if err == nil {
		return nil
	}
	if closeErr := g.srv.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		return fmt.Errorf("xbc: graceful shutdown failed (%v) and forced close failed: %w", err, closeErr)
	}
	return err
}

// toGinChain converts a neutral chain into the gin chain actually registered.
// It is the adapter-side twin of the conversion transport/web/router.go used
// to do itself before the Engine port existed: the router now hands this
// package one already-flattened []web.Handler per route, and this is the
// only place left that crosses back into gin.HandlerFunc.
func (g *engine) toGinChain(chain []web.Handler) []ginlib.HandlerFunc {
	converted := make([]ginlib.HandlerFunc, len(chain))
	for i, handler := range chain {
		converted[i] = web.Handle(handler)
	}
	return converted
}

var (
	_ web.EngineFactory = Factory{}
	_ web.Engine        = (*engine)(nil)
)
