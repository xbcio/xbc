package web

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// Server is the HTTP server Plugin. Its Definition injects the complete
// middleware, route-contributor, route-listener sets, and exactly one Engine
// adapter before the value is constructed; no lifecycle hook scans
// initialized Plugins. Start also assembles the framework-owned stages a
// composition root cannot leave out; the outermost of them is the process-level
// in-flight gate that leads the whole chain (see inflight.go).
type Server struct {
	cfg Config

	factory        EngineFactory
	middlewares    []plugin.Entry[Middleware]
	mappers        []plugin.Entry[ErrorMapper]
	routes         []plugin.Entry[RouteContributor]
	listeners      []plugin.Entry[RouteCatalogListener]
	authenticators []plugin.Entry[authentication.Authenticator]
	extractors     []plugin.Entry[CredentialExtractor]

	// listener is a test-only pre-bound socket set from export_test.go.
	listener net.Listener

	mu               sync.Mutex
	engine           Engine
	router           *Router
	ln               net.Listener
	managementEngine Engine
	managementLn     net.Listener
	ordered          []plugin.Entry[Middleware]
	misses           []MiddlewareOrderMiss
	authentication   *authenticationMiddleware
	catalog          RouteCatalog
	inflight         *inFlightGate
	started          bool
	prepared         bool
	served           bool
}

var (
	_ plugin.Runner        = (*Server)(nil)
	_ plugin.TrafficOpener = (*Server)(nil)
	_ plugin.Preflighter   = (*Server)(nil)
	_ plugin.Closer        = (*Server)(nil)
)

func newServer(
	cfg Config,
	factory EngineFactory,
	middlewares []plugin.Entry[Middleware],
	mappers []plugin.Entry[ErrorMapper],
	routes []plugin.Entry[RouteContributor],
	listeners []plugin.Entry[RouteCatalogListener],
	authenticators []plugin.Entry[authentication.Authenticator],
	extractors []plugin.Entry[CredentialExtractor],
) *Server {
	return &Server{
		cfg:            cfg,
		factory:        factory,
		middlewares:    append([]plugin.Entry[Middleware](nil), middlewares...),
		mappers:        append([]plugin.Entry[ErrorMapper](nil), mappers...),
		routes:         append([]plugin.Entry[RouteContributor](nil), routes...),
		listeners:      append([]plugin.Entry[RouteCatalogListener](nil), listeners...),
		authenticators: append([]plugin.Entry[authentication.Authenticator](nil), authenticators...),
		extractors:     append([]plugin.Entry[CredentialExtractor](nil), extractors...),
	}
}

// Addr returns the bound address after Start succeeds. It resolves an ephemeral
// :0 port before the global traffic gate is released, and it always names the
// serving listener: a deployment that separates the planes has two addresses,
// and this method keeps the meaning every caller already depends on.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// ManagementAddr returns the bound management address after Start succeeds, or
// "" when no management listener is configured -- which is the default, and
// also what a caller sees before Start. Like Addr, it resolves an ephemeral :0
// port, so a caller tests against a real port rather than a guess.
func (s *Server) ManagementAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.managementLn == nil {
		return ""
	}
	return s.managementLn.Addr().String()
}

// assembledPipeline is everything a Server builds before it activates
// anything: the request pipeline, the route table every contributor has
// registered against, and the authentication middleware that is about to
// compile its policy. Start builds it and then goes on to bind a listener and
// submit the serving task; Preflight builds it and stops there, so the fallible
// half of a boot can be exercised -- and reported -- by a process that serves
// nothing.
type assembledPipeline struct {
	cfg            Config
	engine         Engine
	router         *Router
	ordered        []plugin.Entry[Middleware]
	misses         []MiddlewareOrderMiss
	authentication *authenticationMiddleware
	inflight       *inFlightGate
	// managementEngine is the second engine management-plane routes register
	// against, or nil when web.management.addr is unset -- the default, in
	// which there is no second listener and no management route. It is built
	// here, beside the serving engine, because what a plugin registers must be
	// decided before Preflight reports it; the listener it later serves on is
	// bound in Start, and Preflight binds nothing.
	managementEngine Engine
	// tlsConfig is what the listener is wrapped with, or nil for plain HTTP.
	// It is built here -- reading the certificate files -- so Preflight
	// validates the material a boot would terminate TLS with, without binding
	// anything: a certificate that is missing or malformed fails the validate
	// command instead of the first rollout.
	tlsConfig *tls.Config
}

// assemblePipeline builds the immutable request pipeline and registers every
// route the composition contributes, without binding anything or submitting any
// task. Its failure modes are the ones an operator can still act on before
// deploying -- a bad configuration value, a middleware-order conflict, a route
// registration mistake -- which is why it is shared with Preflight rather than
// living inside Start.
//
// The caller owns what happens next: Start installs the result on the Server
// and serves it, Preflight freezes and reports it.
func (s *Server) assemblePipeline(ctx *plugin.Context) (assembledPipeline, error) {
	cfg, err := normalizeConfig(s.cfg)
	if err != nil {
		return assembledPipeline{}, err
	}
	s.cfg = cfg

	logger := ctx.Log()

	tlsConfig, err := cfg.TLS.serverTLSConfig(logger)
	if err != nil {
		return assembledPipeline{}, err
	}

	if s.factory == nil {
		return assembledPipeline{}, errors.New("xbc: web Server has no EngineFactory configured; select an engine Bundle alongside web.Bundle()")
	}
	engine, err := s.factory.NewEngine(Options{
		TrustedProxies:     cfg.TrustedProxies,
		ReadTimeout:        cfg.ReadTimeout,
		ReadHeaderTimeout:  cfg.ReadHeaderTimeout,
		WriteTimeout:       cfg.WriteTimeout,
		IdleTimeout:        cfg.IdleTimeout,
		MaxHeaderBytes:     cfg.MaxHeaderBytes,
		MaxMultipartMemory: cfg.MaxMultipartMemory,
		Logger:             logger,
	})
	if err != nil {
		return assembledPipeline{}, fmt.Errorf("xbc: web engine: %w", err)
	}
	// The management engine is a second engine of the same kind, built from the
	// same options, and it exists exactly when an address was configured for
	// it. Building it here rather than in Start is what lets the routes a
	// plugin registers through Router.Management be decided -- and reported --
	// without binding anything, which is the same split the serving engine
	// already has.
	var managementEngine Engine
	if cfg.Management.Addr != "" {
		managementEngine, err = s.factory.NewEngine(Options{
			TrustedProxies:     cfg.TrustedProxies,
			ReadTimeout:        cfg.ReadTimeout,
			ReadHeaderTimeout:  cfg.ReadHeaderTimeout,
			WriteTimeout:       cfg.WriteTimeout,
			IdleTimeout:        cfg.IdleTimeout,
			MaxHeaderBytes:     cfg.MaxHeaderBytes,
			MaxMultipartMemory: cfg.MaxMultipartMemory,
			Logger:             logger,
		})
		if err != nil {
			return assembledPipeline{}, fmt.Errorf("xbc: web management engine: %w", err)
		}
	}

	routes, frozen, index := newRouteTable()
	// The process-level admission gate is the first element of every chain this
	// Server registers: it precedes capRequestBody, the error boundary, and
	// every contributed middleware, so a request that is going to be refused is
	// refused before any of them run. It is assembled here rather than
	// contributed because an admission ceiling a composition root can leave out
	// is not a ceiling; see inflight.go for the whole argument.
	inflight := newInFlightGate(resolveMaxInFlight(cfg.MaxInFlight), frozen, index)
	handlers := []Handler{
		inflight.handler(logger),
		capRequestBody(cfg.MaxRequestBodyBytes),
		func(_ context.Context, c *Ctx) error {
			newErrorResolver(logger).attach(c)
			return nil
		},
	}

	// The framework's panic boundary, error boundary, and authentication
	// middleware are assembled here rather than selected as plugins. All
	// three are stages the ordering pins below make required, and a required
	// stage must not be something a composition root can omit or an operator
	// can disable: without the panic boundary a handler panic escapes every
	// ordered middleware, without the error boundary every contributed
	// ErrorMapper is unreachable, and without authentication every route is
	// served unauthenticated even though web.security defaults to deny.
	// Enforcing web.security is additionally this Definition's own
	// configuration section, which has exactly one owning plugin. All three
	// still enter the ordering graph under their canonical keys, so the pins
	// resolve against real entries. Those keys are therefore reserved: a
	// contributed middleware claiming one is rejected by rule, before it can
	// surface as a duplicate identity.
	if err := rejectReservedMiddlewareIdentities(s.middlewares); err != nil {
		return assembledPipeline{}, err
	}
	panicGuard := newPanicBoundary(logger, cfg.Recovery.Stack)
	boundary, err := newErrorBoundary(s.mappers)
	if err != nil {
		return assembledPipeline{}, err
	}
	authenticator, err := newAuthenticationMiddleware(cfg.Security, s.authenticators, s.extractors)
	if err != nil {
		return assembledPipeline{}, fmt.Errorf("xbc: web authentication: %w", err)
	}
	middlewares := append(
		[]plugin.Entry[Middleware]{
			{Identity: panicBoundaryIdentity, Value: panicGuard},
			{Identity: errorBoundaryIdentity, Value: boundary},
			{Identity: authenticationIdentity, Value: authenticator},
		},
		s.middlewares...,
	)

	orderOptions := []middlewareOrderOption{
		// The panic boundary must wrap every other PhaseRecover middleware --
		// there should be none, since the phase is reserved for this single
		// framework-owned stage, but the pin still asserts it rather than
		// assuming it.
		pinMiddlewareOutermost(panicBoundaryIdentity),
		// The outer boundary must wrap every focused PhaseError middleware, so
		// an unknown error still reaches Web's safe non-leaking fallbacks.
		pinMiddlewareOutermost(errorBoundaryIdentity),
		// Authentication runs before every other PhaseAuth middleware so
		// authorization always observes a published Principal.
		pinMiddlewareOutermost(authenticationIdentity),
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
		return assembledPipeline{}, err
	}
	for _, entry := range ordered {
		handlers = append(handlers, entry.Value.Handler())
	}
	// The oversized-body rejection closes the framework chain, after every
	// ordered middleware, so a 413 is access-logged, carries a request id, and
	// gets CORS and security headers like any other response. The cap itself is
	// already installed outermost by capRequestBody, so moving the rejection
	// inward costs no safety: nothing downstream can read more than the limit,
	// whether or not this stage has answered yet.
	handlers = append(handlers, rejectOversizedRequestBody(cfg.MaxRequestBodyBytes))
	// An unmatched request is still a request a client made, so the global
	// chain must reach it: CORS, request ids, access logs, metrics, panic
	// recovery and the request-body limit all lose their meaning the moment
	// 404 and 405 slip past them. Splicing it here rather than relying on an
	// engine to do it is the same division of labour the Engine port already
	// has for routes -- the caller hands over one already-flattened chain, and
	// the engine splices nothing.
	//
	// recordCurrentRoute is deliberately absent. Router bakes one per route,
	// keyed by that route's own method and path; an unmatched request has no
	// entry in the frozen table for it to find, and CurrentRoute reporting
	// false is what the authentication middleware already reads as "no handler
	// behind this to protect".
	//
	// The chain is cloned per use: handlers is snapshotted by newRouter below,
	// and appending in place would let a terminal handler land in the spare
	// capacity the Router's own chain grows into.
	unmatchedChain := func(terminal Handler) []Handler {
		return append(slices.Clone(handlers), terminal)
	}
	notFound := func(_ context.Context, c *Ctx) error {
		AbortProblem(c, NewProblem(http.StatusNotFound, "not_found"))
		return nil
	}
	notAllowed := func(_ context.Context, c *Ctx) error {
		AbortProblem(c, NewProblem(http.StatusMethodNotAllowed, "method_not_allowed"))
		return nil
	}
	engine.NoRoute(unmatchedChain(notFound))
	engine.NoMethod(unmatchedChain(notAllowed))

	// The management listener answers the same two questions with the same
	// Problem Details, over the plane's own chain -- see managementPlane for
	// what that chain is and why it is short.
	var management *managementPlane
	if managementEngine != nil {
		planeHandlers := []Handler{panicGuard.Handler()}
		managementEngine.NoRoute(append(slices.Clone(planeHandlers), notFound))
		managementEngine.NoMethod(append(slices.Clone(planeHandlers), notAllowed))
		management = &managementPlane{engine: managementEngine, handlers: planeHandlers}
	}

	// Router snapshots the handlers slice, so this must happen after every
	// entry above is appended.
	router := newRouter(engine, management, cfg.BasePath, handlers, routes, frozen, index)
	// Each contributor registers against its own copy of the root Router. The
	// copy shares the route table (routes, frozen, index), the engine, and the
	// root chain, but keeps its own defaultPerm/defaultAuth -- a default is
	// only meaningful scoped to the registrations that declared it (see the
	// Router field comment), and Router.Perm/Router.Auth mutate the receiver.
	// Handing every contributor the root itself would let the first plugin to
	// call either method set a default for every plugin that registers after
	// it: one plugin's public route would silently make another plugin's
	// routes public, bypassing deny-by-default.
	for _, entry := range s.routes {
		contributor := *router
		entry.Value.RegisterRoutes(&contributor)
	}

	return assembledPipeline{
		cfg:              cfg,
		engine:           engine,
		router:           router,
		ordered:          ordered,
		misses:           misses,
		authentication:   authenticator,
		inflight:         inflight,
		tlsConfig:        tlsConfig,
		managementEngine: managementEngine,
	}, nil
}

// Start assembles the immutable request pipeline, binds the listener, and
// submits the serving loop while managed-task admission is open. The pipeline is
// led by the process-level in-flight gate, the one stage no contributed
// middleware can displace, disable, or order itself outside of. The task waits
// for the runtime-owned traffic gate (or task cancellation) before calling
// Serve, so no ingress is exposed during fallible preparation.
//
// The assembly it starts from is the same one Preflight builds -- see
// assemblePipeline -- so a pipeline validate accepted is the pipeline a boot
// goes on to serve.
func (s *Server) Start(ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("xbc: web Start requires a non-nil plugin context")
	}
	pipeline, err := s.assemblePipeline(ctx)
	if err != nil {
		return err
	}
	cfg := pipeline.cfg
	engine := pipeline.engine
	logger := ctx.Log()

	ln := s.listener
	if ln == nil {
		ln, err = net.Listen("tcp", cfg.Addr)
		if err != nil {
			return fmt.Errorf("xbc: failed to listen on %s: %w", cfg.Addr, err)
		}
	}
	// Wrapping the bound listener -- rather than a TLS-aware branch through the
	// engine -- is what keeps TLS out of the Engine port: adapters accept
	// connections from a listener and never ask whether it speaks TLS. See
	// serverTLSConfig for why HSTS needs no code of ours behind this.
	ln = wrapTLS(ln, pipeline.tlsConfig)

	// The management listener reuses the same TLS configuration, so a
	// deployment terminates TLS with one certificate pair, one client-auth
	// policy, and one reload path on both ports. Its address is the resolved
	// one, not the configured spelling: see ManagementConfig.bindAddr for why a
	// hostless address binds loopback rather than every interface.
	managementEngine := pipeline.managementEngine
	var managementLn net.Listener
	if managementEngine != nil {
		managementLn, err = net.Listen("tcp", cfg.Management.bindAddr())
		if err != nil {
			_ = ln.Close()
			return fmt.Errorf("xbc: failed to listen on management address %s: %w", cfg.Management.bindAddr(), err)
		}
		managementLn = wrapTLS(managementLn, pipeline.tlsConfig)
	}

	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		_ = ln.Close()
		if managementLn != nil {
			_ = managementLn.Close()
		}
		return errors.New("xbc: web Server has already started")
	}
	s.engine = engine
	s.router = pipeline.router
	s.ln = ln
	s.managementEngine = managementEngine
	s.managementLn = managementLn
	s.ordered = pipeline.ordered
	s.misses = pipeline.misses
	s.authentication = pipeline.authentication
	s.inflight = pipeline.inflight
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
		if managementLn != nil {
			_ = managementLn.Close()
		}
		s.mu.Lock()
		s.started = false
		s.ln = nil
		s.managementLn = nil
		s.mu.Unlock()
		return errors.New("xbc: web managed serving task was rejected outside Start admission")
	}
	if managementEngine != nil {
		// The management task waits on the same gate as the serving task, so
		// neither plane answers a request before the runtime opens traffic --
		// and a management scrape cannot observe a process that never finished
		// starting. Its rejection needs no separate rollback: a refused
		// submission leaves nothing admitted (runtime/task.go charges before it
		// creates anything), so the only work to undo is the socket, and the
		// serving task already admitted returns when the runtime cancels this
		// Plugin's task context on the failed Start.
		managementAccepted := ctx.GoCritical(func(taskCtx context.Context) {
			select {
			case <-gate:
			case <-taskCtx.Done():
				return
			}
			if serveErr := managementEngine.Serve(managementLn); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) && !errors.Is(serveErr, net.ErrClosed) {
				logger.Error("xbc: management HTTP service terminated abnormally", "error", serveErr)
			}
		})
		if !managementAccepted {
			_ = ln.Close()
			_ = managementLn.Close()
			s.mu.Lock()
			s.started = false
			s.ln = nil
			s.managementLn = nil
			s.mu.Unlock()
			return errors.New("xbc: web management serving task was rejected outside Start admission")
		}
	}

	// Reported once Start has committed the pipeline, and before ingress is
	// exposed. Nothing has been served through the gate yet, so the rejection
	// count is necessarily zero here: what an operator reads from this line is
	// the ceiling, where it came from, and the Retry-After a refusal will carry.
	// The live reading afterwards is Server.InFlightStats.
	logger.Info(renderInFlightGate(pipeline.inflight.stats(), cfg.MaxInFlight <= 0))
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
	// The address the management listener is bound to, not the configured
	// spelling: the report says "bound", so what it names has to be the port a
	// scrape can actually reach. Empty when no management plane is configured.
	managementAddr := ""
	if s.managementLn != nil {
		managementAddr = s.managementLn.Addr().String()
	}
	s.mu.Unlock()

	catalog, err := freezeRoutes(router, authenticator, listeners)
	if err != nil {
		return err
	}

	logger := log.L()
	if ctx != nil {
		logger = ctx.Log()
	}
	renderStartupReport(logger, s.cfg, managementAddr, ordered, misses, catalog, authenticator)

	s.mu.Lock()
	s.catalog = catalog
	s.prepared = true
	s.mu.Unlock()
	return nil
}

// Preflight assembles and validates what a later Start would build -- the
// request pipeline, every contributed route registration, and the compiled
// authentication policy -- and reports it, without activating any of it: no
// listener is bound, no serving task is admitted, and no request can be served.
//
// The runtime invokes it only for the validate command, whose point is to fail a
// deployment before it is deployed: an unknown authentication scheme, a route no
// policy covers, a middleware ordering conflict, or a route that cannot be
// registered all fail the command, and the report a real boot prints on the way
// to serving is printed here too. What it cannot check is anything that requires
// activation -- that the configured address can actually be bound, for one, is
// still discovered by Start.
func (s *Server) Preflight(ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("xbc: web Preflight requires a non-nil plugin context")
	}
	pipeline, err := s.assemblePipeline(ctx)
	if err != nil {
		return err
	}
	catalog, err := freezeRoutes(pipeline.router, pipeline.authentication, s.listeners)
	if err != nil {
		return err
	}
	// Empty: this report never bound the management listener, and the report
	// says so instead of naming an address it is not serving (see
	// renderManagementPlane).
	renderStartupReport(ctx.Log(), pipeline.cfg, "", pipeline.ordered, pipeline.misses, catalog, pipeline.authentication)
	return nil
}

// freezeRoutes compiles the frozen route table and hands it to every party that
// has to accept it before the runtime releases the traffic gate: the built-in
// authentication middleware first, so that a policy mistake fails startup rather
// than being reported after other listeners have already reacted to the route
// table, and then each contributed listener in composition order.
//
// Nothing here depends on a listener having been bound, which is why Preflight
// can run it for a process that will never serve.
func freezeRoutes(router *Router, authenticator *authenticationMiddleware, listeners []plugin.Entry[RouteCatalogListener]) (RouteCatalog, error) {
	catalog, err := router.freeze()
	if err != nil {
		return nil, err
	}
	if authenticator != nil {
		if err := authenticator.RoutesReady(catalog); err != nil {
			return nil, err
		}
	}
	for _, entry := range listeners {
		if err := entry.Value.RoutesReady(catalog); err != nil {
			return nil, fmt.Errorf("xbc: plugin %s RoutesReady failed: %w", entry.Identity, err)
		}
	}
	return catalog, nil
}

// renderStartupReport prints what a boot decided about its ingress: the ordered
// middleware chain, the routes that landed, and the authentication policy each
// route falls under. OpenTraffic prints it just before the gate opens; Preflight
// prints the same report for a process that will never serve, because the
// decision an operator reads is the same decision either way.
//
// managementAddr says which of the two it is, and travels only into
// renderManagementPlane: a boot's listener is open when the report is written,
// so it passes the address it bound, while a validate run binds nothing and
// passes empty. That difference is one the report has to state rather than
// leave to the reader to infer from the command.
func renderStartupReport(logger log.Logger, cfg Config, managementAddr string, ordered []plugin.Entry[Middleware], misses []MiddlewareOrderMiss, catalog RouteCatalog, authenticator *authenticationMiddleware) {
	if line := renderTLS(cfg.TLS); line != "" {
		logger.Info(line)
	}
	if len(ordered) > 0 {
		logger.Info(renderMiddlewareChain(ordered))
	}
	if len(misses) > 0 {
		logger.Info(renderSoftMisses(misses))
	}
	logger.Info(renderRouteTable(catalog.All()))
	if line := renderManagementPlane(cfg.Management, catalog.All(), managementAddr); line != "" {
		logger.Info(line)
	}
	if authenticator != nil {
		if schemes := authenticator.manager.Schemes(); len(schemes) > 0 {
			logger.Info(renderAuthenticationOrder(schemes))
		}
		logger.Info(renderPublicEndpoints(authenticator.publicRoutes(), authenticator.permitAll))
		logger.Info(renderPolicyDecisions(authenticator.decisions(), authenticator.manager.DefaultSchemes()))
	}
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
	managementEngine := s.managementEngine
	ln := s.ln
	managementLn := s.managementLn
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
		// The management plane drains after the serving plane, so a scrape
		// issued while business traffic is still finishing -- the window an
		// operator watches a rollout through -- lands, and a scrape issued
		// afterwards is refused rather than answered by a process about to
		// exit. Both drains share the runtime's one remaining deadline.
		if managementEngine != nil {
			if err := managementEngine.Shutdown(ctx); err != nil {
				return fmt.Errorf("xbc: management HTTP shutdown failed: %w", err)
			}
		}
		return nil
	}
	for _, listener := range []net.Listener{ln, managementLn} {
		if listener == nil {
			continue
		}
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
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
