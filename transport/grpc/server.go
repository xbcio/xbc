package grpc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/reflection"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// Server is the gRPC server Plugin. Its Definition injects every contribution --
// services, interceptors, authenticators, credential extractors, and status
// mappers -- before the value is constructed; no lifecycle hook scans
// initialized Plugins. Start also assembles the framework-owned stages a
// composition root cannot leave out: the panic boundary, the status boundary,
// and the authentication stage.
type Server struct {
	cfg Config

	contributions contributions

	// listener is a test-only pre-bound socket set from export_test.go.
	listener net.Listener

	mu       sync.Mutex
	server   *grpc.Server
	ln       net.Listener
	assembly assembly
	started  bool
	prepared bool
	served   bool
}

var (
	_ plugin.Runner        = (*Server)(nil)
	_ plugin.TrafficOpener = (*Server)(nil)
	_ plugin.Preflighter   = (*Server)(nil)
	_ plugin.Closer        = (*Server)(nil)
)

// contributions is everything the Definition collects for a Server.
type contributions struct {
	registrars     []plugin.Entry[ServiceRegistrar]
	unary          []plugin.Entry[UnaryInterceptor]
	stream         []plugin.Entry[StreamInterceptor]
	authenticators []plugin.Entry[authentication.Authenticator]
	extractors     []plugin.Entry[CredentialExtractor]
	mappers        []plugin.Entry[StatusMapper]
}

func newServer(cfg Config, c contributions) *Server {
	return &Server{
		cfg: cfg,
		contributions: contributions{
			registrars:     slices.Clone(c.registrars),
			unary:          slices.Clone(c.unary),
			stream:         slices.Clone(c.stream),
			authenticators: slices.Clone(c.authenticators),
			extractors:     slices.Clone(c.extractors),
			mappers:        slices.Clone(c.mappers),
		},
	}
}

// Addr returns the bound address after Start succeeds. It resolves an
// ephemeral :0 port before the traffic gate is released.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// assembly is everything a Server decides before it activates anything: the
// frozen service table, the two ordered interceptor chains, and the TLS
// material when the section declares one. Start builds it and goes on to
// construct and serve a grpc.Server from it; Preflight builds it and stops
// there, so the fallible half of a boot can be exercised -- and reported -- by
// a process that serves nothing.
type assembly struct {
	cfg       Config
	services  []*registeredService
	unary     []plugin.Entry[UnaryInterceptor]
	stream    []plugin.Entry[StreamInterceptor]
	misses    []InterceptorOrderMiss
	auth      *authenticationStage
	tlsConfig *tls.Config
}

// assemble collects and freezes every contributed service, compiles the
// authentication policy of every frozen method, and sorts both interceptor
// chains, without binding anything or submitting any task. Its failure modes --
// a bad configuration value, a duplicate or mistyped service, an unknown or
// unordered scheme, an ordering conflict -- are the ones an operator can still
// act on before deploying, which is why it is shared with Preflight rather than
// living inside Start.
func (s *Server) assemble(ctx *plugin.Context) (assembly, error) {
	cfg, err := normalizeConfig(s.cfg)
	if err != nil {
		return assembly{}, err
	}
	s.cfg = cfg
	c := s.contributions
	logger := ctx.Log()

	// The TLS material is built here, not in Start, so Preflight reads and
	// parses the certificate exactly as a boot would: an unreadable pair is a
	// startup error either way.
	tlsConfig, err := cfg.TLS.serverTLSConfig(logger)
	if err != nil {
		return assembly{}, err
	}

	if err := rejectReservedInterceptorIdentities(c.unary); err != nil {
		return assembly{}, err
	}
	if err := rejectReservedInterceptorIdentities(c.stream); err != nil {
		return assembly{}, err
	}

	services, err := collectServices(c.registrars)
	if err != nil {
		return assembly{}, err
	}
	if cfg.Reflection.Enabled {
		if err := checkReflectionCollisions(services); err != nil {
			return assembly{}, err
		}
	}
	auth, err := newAuthenticationStage(logger, cfg.Security, c.authenticators, c.extractors, services, cfg.Reflection.Enabled)
	if err != nil {
		return assembly{}, err
	}
	mappers, err := orderStatusMappers(c.mappers)
	if err != nil {
		return assembly{}, err
	}

	// One value per stage leads both chains; none holds per-call state.
	recovery := newPanicBoundary(logger, cfg.Recovery.Stack)
	boundary := &statusBoundary{logger: logger, mappers: mappers}
	pins := func(contributed []plugin.Identity) frameworkPins {
		p := frameworkPins{outermost: []plugin.Identity{panicBoundaryIdentity, statusBoundaryIdentity, authenticationIdentity}}
		for _, identity := range contributed {
			p.after = append(p.after, afterPin{interceptor: identity, predecessor: Require(AuthenticationInterceptorKey)})
		}
		return p
	}

	unary, unaryMisses, err := orderInterceptors(unaryChain,
		append([]plugin.Entry[UnaryInterceptor]{
			{Identity: panicBoundaryIdentity, Value: recovery},
			{Identity: statusBoundaryIdentity, Value: boundary},
			{Identity: authenticationIdentity, Value: auth},
		}, c.unary...),
		pins(principalConsumers(c.unary)))
	if err != nil {
		return assembly{}, err
	}
	stream, streamMisses, err := orderInterceptors(streamChain,
		append([]plugin.Entry[StreamInterceptor]{
			{Identity: panicBoundaryIdentity, Value: recovery},
			{Identity: statusBoundaryIdentity, Value: boundary},
			{Identity: authenticationIdentity, Value: auth},
		}, c.stream...),
		pins(principalConsumers(c.stream)))
	if err != nil {
		return assembly{}, err
	}

	return assembly{
		cfg:       cfg,
		services:  services,
		unary:     unary,
		stream:    stream,
		misses:    append(unaryMisses, streamMisses...),
		auth:      auth,
		tlsConfig: tlsConfig,
	}, nil
}

// principalConsumers returns the contributed interceptors that declare they
// need a published Principal. Only contributed entries are scanned, so the
// authentication stage can never be pinned after itself.
func principalConsumers[T ordered](entries []plugin.Entry[T]) []plugin.Identity {
	var identities []plugin.Identity
	for _, entry := range entries {
		if _, ok := any(entry.Value).(authentication.RequiresPrincipal); ok {
			identities = append(identities, entry.Identity)
		}
	}
	return identities
}

// newGRPCServer builds the grpc.Server an assembly describes and installs every
// frozen service on it. The registrations were already checked -- including the
// reflection collision check when reflection is enabled -- so grpc-go's own
// fatal checks cannot fire here.
func newGRPCServer(a assembly) *grpc.Server {
	unary := make([]grpc.UnaryServerInterceptor, len(a.unary))
	for i, entry := range a.unary {
		unary[i] = entry.Value.UnaryInterceptor()
	}
	stream := make([]grpc.StreamServerInterceptor, len(a.stream))
	for i, entry := range a.stream {
		stream[i] = entry.Value.StreamInterceptor()
	}
	options := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(unary...),
		grpc.ChainStreamInterceptor(stream...),
		grpc.MaxRecvMsgSize(a.cfg.MaxRecvMsgBytes),
		grpc.MaxSendMsgSize(a.cfg.MaxSendMsgBytes),
		grpc.MaxConcurrentStreams(a.cfg.MaxConcurrentStreams),
	}
	if a.tlsConfig != nil {
		// The server credentials own the TLS handshake: they reject a peer
		// whose ALPN result is not "h2", bound the handshake by the
		// connection timeout, and publish the negotiated state as the peer's
		// AuthInfo -- none of which a wrapped listener does (see
		// TLSConfig.serverTLSConfig).
		options = append(options, grpc.Creds(credentials.NewTLS(a.tlsConfig)))
	}
	server := grpc.NewServer(options...)
	for _, service := range a.services {
		server.RegisterService(service.desc, service.impl)
	}
	if a.cfg.Reflection.Enabled {
		// The standard services are installed here rather than through the
		// Registrar: no plugin owns them, and their authentication policies
		// were already compiled from the same descriptors.
		reflection.Register(server)
	}
	return server
}

// Start assembles the frozen service table and interceptor chains, binds the
// listener, and submits the serving loop while managed-task admission is open.
// The task waits for the runtime-owned traffic gate (or task cancellation)
// before calling Serve, so no RPC is accepted during fallible preparation.
//
// The assembly it starts from is the same one Preflight builds, so what
// validate accepted is what a boot goes on to serve.
func (s *Server) Start(ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("xbc: gRPC Start requires a non-nil plugin context")
	}
	a, err := s.assemble(ctx)
	if err != nil {
		return err
	}
	logger := ctx.Log()

	ln := s.listener
	if ln == nil {
		ln, err = net.Listen("tcp", a.cfg.Addr)
		if err != nil {
			return fmt.Errorf("xbc: failed to listen on %s: %w", a.cfg.Addr, err)
		}
	}
	server := newGRPCServer(a)

	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		_ = ln.Close()
		server.Stop()
		return errors.New("xbc: gRPC Server has already started")
	}
	s.server = server
	s.ln = ln
	s.assembly = a
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
		if serveErr := server.Serve(ln); serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) {
			logger.Error("xbc: gRPC service terminated abnormally", "error", serveErr)
		}
	})
	if !accepted {
		_ = ln.Close()
		server.Stop()
		s.mu.Lock()
		s.started = false
		s.server = nil
		s.ln = nil
		s.mu.Unlock()
		return errors.New("xbc: gRPC managed serving task was rejected outside Start admission")
	}
	return nil
}

// OpenTraffic reports what this boot decided about its ingress while the
// runtime traffic gate is still closed. The service table was already frozen in
// Start, before the grpc.Server it was installed on existed, so nothing here can
// fail on a started Server.
func (s *Server) OpenTraffic(ctx *plugin.Context) error {
	s.mu.Lock()
	if !s.started || s.server == nil {
		s.mu.Unlock()
		return errors.New("xbc: gRPC Server has not started successfully; cannot prepare traffic")
	}
	if s.prepared {
		s.mu.Unlock()
		return nil
	}
	a := s.assembly
	addr := s.ln.Addr().String()
	s.prepared = true
	s.mu.Unlock()

	logger := log.L()
	if ctx != nil {
		logger = ctx.Log()
	}
	renderStartupReport(logger, addr, a)
	return nil
}

// Preflight assembles and validates what a later Start would build -- the
// frozen service table and both interceptor chains -- and reports it, without
// activating any of it: no listener is bound, no grpc.Server is constructed,
// and no serving task is admitted. That the configured address can actually be
// bound is still discovered by Start.
func (s *Server) Preflight(ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("xbc: gRPC Preflight requires a non-nil plugin context")
	}
	a, err := s.assemble(ctx)
	if err != nil {
		return err
	}
	renderStartupReport(ctx.Log(), "", a)
	return nil
}

// Stop drains a serving server or closes a listener that never crossed the
// traffic gate. It is safe after every partially completed owned state.
//
// A serving server stops gracefully -- no new RPCs, in-flight ones run to
// completion -- for as long as ctx allows. When ctx ends first, every
// connection is closed and the remaining RPCs' contexts are cancelled with
// grpc.Server.Stop, and the expiry is reported without waiting for handlers
// that ignore cancellation: the runtime owns the one shutdown budget, and the
// transport does not extend it.
func (s *Server) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	started := s.started
	served := s.served
	server := s.server
	ln := s.ln
	preDrainDelay := s.cfg.Shutdown.PreDrainDelay
	s.mu.Unlock()
	if !started || server == nil {
		return nil
	}

	if !served {
		// Serve was never called, so grpc-go does not own the listener; close it
		// here, then release the server's own resources.
		var closeErr error
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			closeErr = fmt.Errorf("xbc: failed to close gRPC listener before traffic gate release: %w", err)
		}
		server.Stop()
		return closeErr
	}

	waitForPreDrain(ctx, preDrainDelay)
	drained := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(drained)
	}()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		// Stop closes every connection and cancels every RPC context, but it
		// cannot make a handler that ignores its context return. Worse, once
		// the graceful stop above has seen its connections close, grpc-go
		// waits for handlers while holding the lock Stop needs, so a forced
		// stop issued from here can block on that same handler. It therefore
		// runs on its own goroutine: the connections are closed either way --
		// by Stop, or already, if the graceful stop got that far -- and Stop
		// returns under the deadline the runtime set instead of extending it.
		go server.Stop()
		return fmt.Errorf("xbc: gRPC graceful stop did not finish before the shutdown deadline; remaining RPCs were cancelled: %w", ctx.Err())
	}
}

// waitForPreDrain preserves an externally reachable readiness window without
// inventing a transport-local shutdown budget. A cancelled deadline ends the
// window immediately so the graceful stop that follows runs under the same
// runtime budget.
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
