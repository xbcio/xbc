package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/authentication"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// Server is the Gin-backed HTTP server Plugin. Its Definition injects the
// complete middleware, route-contributor, and route-listener sets before the
// value is constructed; no lifecycle hook scans initialized Plugins.
type Server struct {
	cfg Config

	middlewares []plugin.Entry[Middleware]
	routes      []plugin.Entry[RouteContributor]
	listeners   []plugin.Entry[RouteCatalogListener]

	// listener is a test-only pre-bound socket set from export_test.go.
	listener net.Listener

	mu       sync.Mutex
	engine   *gin.Engine
	router   *Router
	ln       net.Listener
	srv      *http.Server
	ordered  []plugin.Entry[Middleware]
	misses   []MiddlewareOrderMiss
	catalog  RouteCatalog
	started  bool
	prepared bool
	served   bool
}

var (
	_ plugin.Runner        = (*Server)(nil)
	_ plugin.TrafficOpener = (*Server)(nil)
	_ plugin.Closer        = (*Server)(nil)
)

func newServer(
	cfg Config,
	middlewares []plugin.Entry[Middleware],
	routes []plugin.Entry[RouteContributor],
	listeners []plugin.Entry[RouteCatalogListener],
) *Server {
	return &Server{
		cfg:         cfg,
		middlewares: append([]plugin.Entry[Middleware](nil), middlewares...),
		routes:      append([]plugin.Entry[RouteContributor](nil), routes...),
		listeners:   append([]plugin.Entry[RouteCatalogListener](nil), listeners...),
	}
}

// Addr returns the bound address after Start succeeds. It resolves an ephemeral
// :0 port before the global traffic gate is released.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

type ginLogWriter struct {
	logger log.Logger
	level  log.Level
}

func (w ginLogWriter) Write(p []byte) (int, error) {
	message := strings.TrimRight(string(p), "\n")
	if message != "" {
		if w.level == log.ErrorLevel {
			w.logger.Error(message)
		} else {
			w.logger.Info(message)
		}
	}
	return len(p), nil
}

func setGinMode(logger log.Logger) {
	if logger != nil && logger.Enabled(log.DebugLevel) {
		gin.SetMode(gin.DebugMode)
		return
	}
	gin.SetMode(gin.ReleaseMode)
}

// Start assembles the immutable request pipeline, binds the listener, and
// submits the serving loop while managed-task admission is open. The task waits
// for the runtime-owned traffic gate (or task cancellation) before calling
// Serve, so no ingress is exposed during fallible preparation.
func (s *Server) Start(ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("xbc: web Start requires a non-nil plugin context")
	}
	cfg, err := normalizeConfig(s.cfg)
	if err != nil {
		return err
	}
	s.cfg = cfg

	logger := ctx.Log()
	setGinMode(logger)
	gin.DefaultWriter = ginLogWriter{logger: logger, level: log.InfoLevel}
	gin.DefaultErrorWriter = ginLogWriter{logger: logger, level: log.ErrorLevel}

	engine := gin.New()
	if err := engine.SetTrustedProxies(cfg.TrustedProxies); err != nil {
		return fmt.Errorf("xbc: web trusted_proxies: %w", err)
	}
	engine.HandleMethodNotAllowed = true
	engine.MaxMultipartMemory = cfg.MaxMultipartMemory

	routes, frozen, index := newRouteTable()
	engine.Use(recordCurrentRoute(frozen, index))
	engine.Use(limitRequestBody(cfg.MaxRequestBodyBytes))
	engine.Use(newErrorResolver(logger).attach)

	orderOptions := []middlewareOrderOption{
		pinMiddlewareOutermost(Require(ErrorBoundaryKey)),
	}
	for _, entry := range s.middlewares {
		if _, requiresPrincipal := entry.Value.(authentication.RequiresPrincipal); requiresPrincipal {
			orderOptions = append(orderOptions,
				pinMiddlewareAfter(entry.Identity, Require(AuthenticationMiddlewareKey)))
		}
	}
	ordered, misses, err := orderMiddlewares(s.middlewares, orderOptions...)
	if err != nil {
		return err
	}
	for _, entry := range ordered {
		engine.Use(entry.Value.Handler())
	}
	engine.NoRoute(func(c *gin.Context) {
		AbortProblem(c, NewProblem(http.StatusNotFound, "not_found"))
	})
	engine.NoMethod(func(c *gin.Context) {
		AbortProblem(c, NewProblem(http.StatusMethodNotAllowed, "method_not_allowed"))
	})

	// Group snapshots the engine middleware slice, so this must happen after
	// every Use call above.
	router := newRouter(engine, cfg.BasePath, routes, frozen, index)
	for _, entry := range s.routes {
		entry.Value.RegisterRoutes(router)
	}

	ln := s.listener
	if ln == nil {
		ln, err = net.Listen("tcp", cfg.Addr)
		if err != nil {
			return fmt.Errorf("xbc: failed to listen on %s: %w", cfg.Addr, err)
		}
	}
	srv := &http.Server{
		Handler:           engine,
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
	}

	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		_ = ln.Close()
		return errors.New("xbc: web Server has already started")
	}
	s.engine = engine
	s.router = router
	s.ln = ln
	s.srv = srv
	s.ordered = ordered
	s.misses = misses
	s.started = true
	s.mu.Unlock()

	gate := ctx.TrafficGate()
	accepted := ctx.GoCritical(func(taskCtx context.Context) {
		select {
		case <-gate:
		case <-taskCtx.Done():
			return
		}

		s.mu.Lock()
		s.served = true
		s.mu.Unlock()
		if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) && !errors.Is(serveErr, net.ErrClosed) {
			logger.Error("xbc: HTTP service terminated abnormally", "error", serveErr)
		}
	})
	if !accepted {
		_ = ln.Close()
		s.mu.Lock()
		s.started = false
		s.ln = nil
		s.mu.Unlock()
		return errors.New("xbc: web managed serving task was rejected outside Start admission")
	}
	return nil
}

// OpenTraffic performs the remaining fallible preparation while the runtime
// traffic gate is still closed: route-policy validation/freeze and listener
// notification. The runtime atomically closes the gate only after every
// participant's OpenTraffic succeeds.
func (s *Server) OpenTraffic(ctx *plugin.Context) error {
	s.mu.Lock()
	if !s.started || s.router == nil {
		s.mu.Unlock()
		return errors.New("xbc: web Server has not started successfully; cannot prepare traffic")
	}
	if s.prepared {
		s.mu.Unlock()
		return nil
	}
	router := s.router
	ordered := append([]plugin.Entry[Middleware](nil), s.ordered...)
	misses := append([]MiddlewareOrderMiss(nil), s.misses...)
	listeners := append([]plugin.Entry[RouteCatalogListener](nil), s.listeners...)
	s.mu.Unlock()

	catalog, err := router.freeze()
	if err != nil {
		return err
	}
	for _, entry := range listeners {
		if err := entry.Value.RoutesReady(catalog); err != nil {
			return fmt.Errorf("xbc: plugin %s RoutesReady failed: %w", entry.Identity, err)
		}
	}

	logger := log.L()
	if ctx != nil {
		logger = ctx.Log()
	}
	if len(ordered) > 0 {
		logger.Info(renderMiddlewareChain(ordered))
	}
	if len(misses) > 0 {
		logger.Info(renderSoftMisses(misses))
	}
	logger.Info(renderRouteTable(catalog.All()))

	s.mu.Lock()
	s.catalog = catalog
	s.prepared = true
	s.mu.Unlock()
	return nil
}

// Stop drains a serving server or closes a listener that never crossed the
// global traffic gate. It is safe after every partially completed owned state.
func (s *Server) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	started := s.started
	served := s.served
	srv := s.srv
	ln := s.ln
	s.mu.Unlock()
	if !started {
		return nil
	}

	if served && srv != nil {
		if err := srv.Shutdown(ctx); err != nil {
			if closeErr := srv.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				return fmt.Errorf("xbc: graceful shutdown failed (%v) and forced close failed: %w", err, closeErr)
			}
			return err
		}
		return nil
	}
	if ln != nil {
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			return fmt.Errorf("xbc: failed to close listener before traffic gate release: %w", err)
		}
	}
	return nil
}
