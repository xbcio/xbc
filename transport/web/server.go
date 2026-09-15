package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// Server is the HTTP server Plugin. Its Definition injects the complete
// middleware, route-contributor, route-listener sets, and exactly one Engine
// adapter before the value is constructed; no lifecycle hook scans
// initialized Plugins.
type Server struct {
	cfg Config

	factory        EngineFactory
	middlewares    []plugin.Entry[Middleware]
	routes         []plugin.Entry[RouteContributor]
	listeners      []plugin.Entry[RouteCatalogListener]
	authenticators []plugin.Entry[authentication.Authenticator]
	extractors     []plugin.Entry[CredentialExtractor]

	// listener is a test-only pre-bound socket set from export_test.go.
	listener net.Listener

	mu             sync.Mutex
	engine         Engine
	router         *Router
	ln             net.Listener
	ordered        []plugin.Entry[Middleware]
	misses         []MiddlewareOrderMiss
	authentication *authenticationMiddleware
	catalog        RouteCatalog
	started        bool
	prepared       bool
	served         bool
}

var (
	_ plugin.Runner        = (*Server)(nil)
	_ plugin.TrafficOpener = (*Server)(nil)
	_ plugin.Closer        = (*Server)(nil)
)

func newServer(
	cfg Config,
	factory EngineFactory,
	middlewares []plugin.Entry[Middleware],
	routes []plugin.Entry[RouteContributor],
	listeners []plugin.Entry[RouteCatalogListener],
	authenticators []plugin.Entry[authentication.Authenticator],
	extractors []plugin.Entry[CredentialExtractor],
) *Server {
	return &Server{
		cfg:            cfg,
		factory:        factory,
		middlewares:    append([]plugin.Entry[Middleware](nil), middlewares...),
		routes:         append([]plugin.Entry[RouteContributor](nil), routes...),
		listeners:      append([]plugin.Entry[RouteCatalogListener](nil), listeners...),
		authenticators: append([]plugin.Entry[authentication.Authenticator](nil), authenticators...),
		extractors:     append([]plugin.Entry[CredentialExtractor](nil), extractors...),
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

	if s.factory == nil {
		return errors.New("xbc: web Server has no EngineFactory configured; select an engine Bundle alongside web.Bundle()")
	}
	engine, err := s.factory.NewEngine(Options{
		TrustedProxies:         cfg.TrustedProxies,
		ReadTimeout:            cfg.ReadTimeout,
		ReadHeaderTimeout:      cfg.ReadHeaderTimeout,
		WriteTimeout:           cfg.WriteTimeout,
		IdleTimeout:            cfg.IdleTimeout,
		MaxHeaderBytes:         cfg.MaxHeaderBytes,
		MaxMultipartMemory:     cfg.MaxMultipartMemory,
		HandleMethodNotAllowed: true,
	})
	if err != nil {
		return fmt.Errorf("xbc: web engine: %w", err)
	}

	routes, frozen, index := newRouteTable()
	handlers := []Handler{
		limitRequestBody(cfg.MaxRequestBodyBytes),
		func(_ context.Context, c *Ctx) error {
			newErrorResolver(logger).attach(c)
			return nil
		},
	}

	// The framework's authentication middleware is assembled here rather than
	// selected as a plugin: it enforces this Server's own web.security section,
	// and a configuration section has exactly one owning plugin. It still enters
	// the ordering graph under AuthenticationMiddlewareKey, so the pins below
	// resolve against a real entry.
	authenticator, err := newAuthenticationMiddleware(cfg.Security, s.authenticators, s.extractors)
	if err != nil {
		return fmt.Errorf("xbc: web authentication: %w", err)
	}
	middlewares := append(
		[]plugin.Entry[Middleware]{{Identity: authenticationIdentity, Value: authenticator}},
		s.middlewares...,
	)

	orderOptions := []middlewareOrderOption{
		pinMiddlewareOutermost(Require(ErrorBoundaryKey)),
		// Authentication runs before every other PhaseAuth middleware so
		// authorization always observes a published Principal.
		pinMiddlewareOutermost(Require(AuthenticationMiddlewareKey)),
	}
	// Only contributed middleware can require a principal. Scanning the built-in
	// entry too would let a future edit pin authentication after itself.
	for _, entry := range s.middlewares {
		if _, requiresPrincipal := entry.Value.(authentication.RequiresPrincipal); requiresPrincipal {
			orderOptions = append(orderOptions,
				pinMiddlewareAfter(entry.Identity, Require(AuthenticationMiddlewareKey)))
		}
	}
	ordered, misses, err := orderMiddlewares(middlewares, orderOptions...)
	if err != nil {
		return err
	}
	for _, entry := range ordered {
		handlers = append(handlers, entry.Value.Handler())
	}
	engine.NoRoute([]Handler{func(_ context.Context, c *Ctx) error {
		AbortProblem(c, NewProblem(http.StatusNotFound, "not_found"))
		return nil
	}})
	engine.NoMethod([]Handler{func(_ context.Context, c *Ctx) error {
		AbortProblem(c, NewProblem(http.StatusMethodNotAllowed, "method_not_allowed"))
		return nil
	}})

	// Router snapshots the handlers slice, so this must happen after every
	// entry above is appended.
	router := newRouter(engine, cfg.BasePath, handlers, routes, frozen, index)
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

	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		_ = ln.Close()
		return errors.New("xbc: web Server has already started")
	}
	s.engine = engine
	s.router = router
	s.ln = ln
	s.ordered = ordered
	s.misses = misses
	s.authentication = authenticator
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
		if serveErr := engine.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) && !errors.Is(serveErr, net.ErrClosed) {
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
	authenticator := s.authentication
	s.mu.Unlock()

	catalog, err := router.freeze()
	if err != nil {
		return err
	}
	// The built-in authentication middleware compiles its policy table and runs
	// the startup policy validations before any contributed listener observes
	// the catalog: a policy mistake must fail startup, not be reported after
	// other listeners have already reacted to the route table.
	if authenticator != nil {
		if err := authenticator.RoutesReady(catalog); err != nil {
			return err
		}
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
	if authenticator != nil {
		logger.Info(renderPublicEndpoints(authenticator.publicRoutes(), authenticator.permitAll))
		logger.Info(renderPolicyDecisions(authenticator.decisions()))
	}

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
	engine := s.engine
	ln := s.ln
	preDrainDelay := s.cfg.Shutdown.PreDrainDelay
	s.mu.Unlock()
	if !started {
		return nil
	}

	if served && engine != nil {
		// Runtime cancels every lifecycle context before it starts the reverse
		// cleanup walk. When a health plugin is selected, its readiness route
		// therefore reports Down here while this listener still serves probes.
		// Shutdown itself closes listeners immediately, so the propagation window
		// must precede it. It consumes only the runtime-owned remaining deadline.
		waitForPreDrain(ctx, preDrainDelay)
		if err := engine.Shutdown(ctx); err != nil {
			return fmt.Errorf("xbc: graceful shutdown failed: %w", err)
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

// waitForPreDrain preserves an externally reachable readiness window without
// inventing a Web-local shutdown budget. A cancelled deadline ends the window
// immediately so the subsequent Shutdown can force-close the listener under
// the same runtime budget.
func waitForPreDrain(ctx context.Context, delay time.Duration) {
	if delay <= 0 {
		return
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}
