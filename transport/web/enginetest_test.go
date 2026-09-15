package web

import (
	"context"
	"net"
	"net/http"

	"github.com/gin-gonic/gin"
)

// testEngine and testEngineFactory stand in for engines/gin's real adapter in
// this package's own tests. engines/gin necessarily imports web to implement
// web.Engine against web.Handler, so web's internal ("package web") tests
// cannot import engines/gin back without an illegal import cycle. testEngine
// mirrors that adapter's gin-wrapping closely enough to exercise Router,
// Server.Start, and Server.Stop against a real gin.Engine and http.Server.
type testEngine struct {
	gin *gin.Engine
	srv *http.Server
}

func (e *testEngine) Handle(method, path string, chain []Handler) {
	e.gin.Handle(method, path, e.toGinChain(chain)...)
}

func (e *testEngine) NoRoute(chain []Handler)  { e.gin.NoRoute(e.toGinChain(chain)...) }
func (e *testEngine) NoMethod(chain []Handler) { e.gin.NoMethod(e.toGinChain(chain)...) }

func (e *testEngine) Serve(ln net.Listener) error        { return e.srv.Serve(ln) }
func (e *testEngine) Shutdown(ctx context.Context) error { return e.srv.Shutdown(ctx) }

func (e *testEngine) toGinChain(chain []Handler) []gin.HandlerFunc {
	converted := make([]gin.HandlerFunc, len(chain))
	for i, handler := range chain {
		converted[i] = Handle(handler)
	}
	return converted
}

// ServeHTTP lets tests dispatch a request straight at the test engine, the
// same way they used to dispatch straight at a *gin.Engine.
func (e *testEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.gin.ServeHTTP(w, r)
}

type testEngineFactory struct{}

func (testEngineFactory) NewEngine(opts Options) (Engine, error) {
	e := gin.New()
	if err := e.SetTrustedProxies(opts.TrustedProxies); err != nil {
		return nil, err
	}
	e.HandleMethodNotAllowed = opts.HandleMethodNotAllowed
	e.MaxMultipartMemory = opts.MaxMultipartMemory
	return &testEngine{
		gin: e,
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

// newTestEngine builds a bare testEngine directly, for router tests that only
// need an Engine and never go through Server.Start/Options.
func newTestEngine() *testEngine {
	gin.SetMode(gin.TestMode)
	e, err := (testEngineFactory{}).NewEngine(Options{})
	if err != nil {
		panic(err)
	}
	return e.(*testEngine)
}

var (
	_ EngineFactory = testEngineFactory{}
	_ Engine        = (*testEngine)(nil)
)
