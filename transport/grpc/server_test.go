package grpc

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/xbcio/xbc/plugin"
)

// echoServer is the handler interface of a hand-written test service. The
// descriptor below has the shape protoc-gen-go-grpc generates, so the Server
// sees exactly what a generated registration would hand it.
type echoServer interface {
	Echo(context.Context, *wrapperspb.StringValue) (*wrapperspb.StringValue, error)
	Repeat(*wrapperspb.StringValue, grpc.ServerStream) error
}

const echoServiceName = "xbc.test.Echo"

func echoDesc() *grpc.ServiceDesc {
	return &grpc.ServiceDesc{
		ServiceName: echoServiceName,
		HandlerType: (*echoServer)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Echo",
			Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				in := new(wrapperspb.StringValue)
				if err := dec(in); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return srv.(echoServer).Echo(ctx, in)
				}
				info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + echoServiceName + "/Echo"}
				return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
					return srv.(echoServer).Echo(ctx, req.(*wrapperspb.StringValue))
				})
			},
		}},
		Streams: []grpc.StreamDesc{{
			StreamName:    "Repeat",
			ServerStreams: true,
			Handler: func(srv any, stream grpc.ServerStream) error {
				in := new(wrapperspb.StringValue)
				if err := stream.RecvMsg(in); err != nil {
					return err
				}
				return srv.(echoServer).Repeat(in, stream)
			},
		}},
	}
}

type echoImpl struct {
	panicOn string
	// release, when set, holds every Echo call until it is closed; entered is
	// signalled as each held call arrives, so a stop test knows an RPC is in
	// flight before it stops the server.
	release chan struct{}
	entered chan struct{}
}

func blockingEcho() *echoImpl {
	return &echoImpl{release: make(chan struct{}), entered: make(chan struct{}, 1)}
}

func (e *echoImpl) Echo(_ context.Context, in *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
	if e.panicOn != "" && in.GetValue() == e.panicOn {
		panic("secret-token-value")
	}
	if e.release != nil {
		e.entered <- struct{}{}
		<-e.release
	}
	return wrapperspb.String("echo:" + in.GetValue()), nil
}

func (e *echoImpl) Repeat(in *wrapperspb.StringValue, stream grpc.ServerStream) error {
	for range 2 {
		if err := stream.SendMsg(wrapperspb.String(in.GetValue())); err != nil {
			return err
		}
	}
	return nil
}

type registrarFunc func(*Registrar) error

func (f registrarFunc) RegisterServices(r *Registrar) error { return f(r) }

// echoContributor registers the echo service as public, so the lifecycle and
// ordering tests do not depend on an authenticator. Authentication has its own
// tests in authentication_test.go.
func echoContributor(impl echoServer) plugin.Entry[ServiceRegistrar] {
	return echoContributorWith(impl, Public())
}

func echoContributorWith(impl echoServer, policy AuthPolicy) plugin.Entry[ServiceRegistrar] {
	return plugin.Entry[ServiceRegistrar]{
		Identity: plugin.Identity{Plugin: "echo"},
		Value: registrarFunc(func(r *Registrar) error {
			r.RegisterService(echoDesc(), impl)
			r.Methods(echoDesc().ServiceName).Auth(policy)
			return nil
		}),
	}
}

// recorder records the order interceptors run in, across both chains.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) add(call string) {
	r.mu.Lock()
	r.calls = append(r.calls, call)
	r.mu.Unlock()
}

func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	calls := r.calls
	r.calls = nil
	return calls
}

type testInterceptor struct {
	name  string
	order Order
	rec   *recorder
}

func (i testInterceptor) Order() Order { return i.order }

func (i testInterceptor) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		i.rec.add("unary:" + i.name)
		return handler(ctx, req)
	}
}

func (i testInterceptor) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		i.rec.add("stream:" + i.name)
		return handler(srv, stream)
	}
}

func unaryEntry(key plugin.Key, i testInterceptor) plugin.Entry[UnaryInterceptor] {
	return plugin.Entry[UnaryInterceptor]{Identity: plugin.Identity{Plugin: key}, Value: i}
}

func streamEntry(key plugin.Key, i testInterceptor) plugin.Entry[StreamInterceptor] {
	return plugin.Entry[StreamInterceptor]{Identity: plugin.Identity{Plugin: key}, Value: i}
}

func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// startServing runs Start and OpenTraffic and releases the gate, returning a
// client connection to the serving listener.
func startServing(t *testing.T, s *Server) (*fakeHost, *grpc.ClientConn) {
	t.Helper()
	s.listener = listenLoopback(t)
	host := newFakeHost()
	ctx := contextFromHost(host)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.OpenTraffic(ctx); err != nil {
		t.Fatalf("OpenTraffic: %v", err)
	}
	host.releaseTraffic()
	conn, err := grpc.NewClient("passthrough:///"+s.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Stop(stopCtx)
		host.shutdown()
	})
	return host, conn
}

func callEcho(t *testing.T, conn *grpc.ClientConn, value string) (*wrapperspb.StringValue, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out := new(wrapperspb.StringValue)
	err := conn.Invoke(ctx, "/"+echoServiceName+"/Echo", wrapperspb.String(value), out)
	return out, err
}

func callRepeat(t *testing.T, conn *grpc.ClientConn, value string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	desc := &grpc.StreamDesc{StreamName: "Repeat", ServerStreams: true}
	stream, err := conn.NewStream(ctx, desc, "/"+echoServiceName+"/Repeat")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SendMsg(wrapperspb.String(value)); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	var got []string
	for {
		msg := new(wrapperspb.StringValue)
		if err := stream.RecvMsg(msg); err != nil {
			if errors.Is(err, io.EOF) {
				return got
			}
			t.Fatal(err)
		}
		got = append(got, msg.GetValue())
	}
}

func equalStrings(a, b []string) bool {
	return strings.Join(a, "|") == strings.Join(b, "|")
}

func TestServerServesUnaryAndStreamThroughSeparatelyOrderedChains(t *testing.T) {
	rec := &recorder{}
	// Unary: auth before observe would violate phase, so order inside one
	// phase is decided by Before/After, and across phases by Phase alone.
	// "zeta" sorts after "alpha" by identity but is declared Before it.
	unary := []plugin.Entry[UnaryInterceptor]{
		unaryEntry("alpha", testInterceptor{name: "alpha", order: Order{Phase: PhaseObserve}, rec: rec}),
		unaryEntry("zeta", testInterceptor{name: "zeta", order: Order{Phase: PhaseObserve, Before: []OrderRef{Require("alpha")}}, rec: rec}),
		unaryEntry("auth", testInterceptor{name: "auth", order: Order{Phase: PhaseAuth}, rec: rec}),
	}
	// Stream: a different chain with its own order; "alpha" is absent here, so
	// the same Prefer reference is a soft miss for stream only.
	stream := []plugin.Entry[StreamInterceptor]{
		streamEntry("auth", testInterceptor{name: "auth", order: Order{Phase: PhaseAuth}, rec: rec}),
		streamEntry("beta", testInterceptor{name: "beta", order: Order{Phase: PhaseSecurity, After: []OrderRef{Prefer("alpha")}}, rec: rec}),
	}
	s := newServer(DefaultConfig(), contributions{registrars: []plugin.Entry[ServiceRegistrar]{echoContributor(&echoImpl{})}, unary: unary, stream: stream})
	_, conn := startServing(t, s)

	out, err := callEcho(t, conn, "hi")
	if err != nil || out.GetValue() != "echo:hi" {
		t.Fatalf("Echo = %v, %v; want echo:hi", out, err)
	}
	if got, want := rec.take(), []string{"unary:zeta", "unary:alpha", "unary:auth"}; !equalStrings(got, want) {
		t.Fatalf("unary chain ran %v, want %v", got, want)
	}

	if got, want := callRepeat(t, conn, "x"), []string{"x", "x"}; !equalStrings(got, want) {
		t.Fatalf("Repeat = %v, want %v", got, want)
	}
	if got, want := rec.take(), []string{"stream:beta", "stream:auth"}; !equalStrings(got, want) {
		t.Fatalf("stream chain ran %v, want %v", got, want)
	}

	misses := s.assembly.misses
	if len(misses) != 1 || misses[0].Chain != "stream" || misses[0].Reference.Key() != "alpha" {
		t.Fatalf("misses = %v, want one stream miss for alpha", misses)
	}
	if s.assembly.unary[0].Identity != panicBoundaryIdentity || s.assembly.stream[0].Identity != panicBoundaryIdentity {
		t.Fatal("the panic boundary must lead both chains")
	}
}

func TestOrderingIsIndependentOfCollectionOrder(t *testing.T) {
	rec := &recorder{}
	a := unaryEntry("a", testInterceptor{name: "a", order: Order{Phase: PhaseBusiness}, rec: rec})
	b := unaryEntry("b", testInterceptor{name: "b", order: Order{Phase: PhaseBusiness}, rec: rec})
	c := unaryEntry("c", testInterceptor{name: "c", order: Order{Phase: PhaseObserve}, rec: rec})
	first, _, err := orderInterceptors(unaryChain, []plugin.Entry[UnaryInterceptor]{a, b, c}, frameworkPins{})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := orderInterceptors(unaryChain, []plugin.Entry[UnaryInterceptor]{b, c, a}, frameworkPins{})
	if err != nil {
		t.Fatal(err)
	}
	names := func(entries []plugin.Entry[UnaryInterceptor]) []string {
		var out []string
		for _, entry := range entries {
			out = append(out, entry.Identity.String())
		}
		return out
	}
	if want := []string{"c", "a", "b"}; !equalStrings(names(first), want) || !equalStrings(names(second), want) {
		t.Fatalf("orders = %v and %v, want %v for both", names(first), names(second), want)
	}
}

func TestOrderingRejectsCyclesPhaseConflictsAndMissingRequiredTargets(t *testing.T) {
	rec := &recorder{}
	cases := []struct {
		name    string
		entries []plugin.Entry[StreamInterceptor]
		check   func(error) bool
	}{
		{
			name: "cycle",
			entries: []plugin.Entry[StreamInterceptor]{
				streamEntry("a", testInterceptor{order: Order{Phase: PhaseAuth, Before: []OrderRef{Require("b")}}, rec: rec}),
				streamEntry("b", testInterceptor{order: Order{Phase: PhaseAuth, Before: []OrderRef{Require("a")}}, rec: rec}),
			},
			check: func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "stream interceptor ordering failed")
			},
		},
		{
			name: "phase conflict",
			entries: []plugin.Entry[StreamInterceptor]{
				streamEntry("a", testInterceptor{order: Order{Phase: PhaseAuth, Before: []OrderRef{Require("b")}}, rec: rec}),
				streamEntry("b", testInterceptor{order: Order{Phase: PhaseObserve}, rec: rec}),
			},
			check: func(err error) bool {
				var target *InterceptorPhaseConflictError
				return errors.As(err, &target)
			},
		},
		{
			name: "missing required",
			entries: []plugin.Entry[StreamInterceptor]{
				streamEntry("a", testInterceptor{order: Order{Phase: PhaseAuth, After: []OrderRef{Require("absent")}}, rec: rec}),
			},
			check: func(err error) bool {
				var target *MissingInterceptorOrderTargetError
				return errors.As(err, &target) && target.Chain == "stream"
			},
		},
		{
			name: "below recovery",
			entries: []plugin.Entry[StreamInterceptor]{
				streamEntry("a", testInterceptor{order: Order{Phase: PhaseRecover - 1}, rec: rec}),
			},
			check: func(err error) bool {
				var target *InterceptorPhaseOutsideRecoveryError
				return errors.As(err, &target)
			},
		},
		{
			name: "duplicate identity",
			entries: []plugin.Entry[StreamInterceptor]{
				streamEntry("a", testInterceptor{order: Order{Phase: PhaseAuth}, rec: rec}),
				streamEntry("a", testInterceptor{order: Order{Phase: PhaseAuth}, rec: rec}),
			},
			check: func(err error) bool {
				var target *DuplicateInterceptorIdentityError
				return errors.As(err, &target)
			},
		},
		{
			name: "reserved key",
			entries: []plugin.Entry[StreamInterceptor]{
				streamEntry(PanicBoundaryKey, testInterceptor{order: Order{Phase: PhaseRecover}, rec: rec}),
			},
			check: func(err error) bool {
				var target *ReservedInterceptorIdentityError
				return errors.As(err, &target)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(DefaultConfig(), contributions{stream: tc.entries})
			err := s.Preflight(contextFromHost(newFakeHost()))
			if !tc.check(err) {
				t.Fatalf("Preflight error = %v", err)
			}
		})
	}
}

func TestPanicBoundaryReturnsInternalWithoutLeakingThePanicValue(t *testing.T) {
	s := newServer(DefaultConfig(), contributions{registrars: []plugin.Entry[ServiceRegistrar]{echoContributor(&echoImpl{panicOn: "boom"})}})
	_, conn := startServing(t, s)
	_, err := callEcho(t, conn, "boom")
	if status.Code(err) != codes.Internal {
		t.Fatalf("Echo(boom) error = %v, want codes.Internal", err)
	}
	if strings.Contains(err.Error(), "secret-token-value") {
		t.Fatalf("panic value leaked to the client: %v", err)
	}
	// The process survived and keeps serving.
	if out, err := callEcho(t, conn, "ok"); err != nil || out.GetValue() != "echo:ok" {
		t.Fatalf("Echo after panic = %v, %v", out, err)
	}
}

func TestDuplicateServiceFailsStartupWithBothContributors(t *testing.T) {
	first := echoContributor(&echoImpl{})
	second := echoContributor(&echoImpl{})
	second.Identity = plugin.Identity{Plugin: "echo-copy"}
	s := newServer(DefaultConfig(), contributions{registrars: []plugin.Entry[ServiceRegistrar]{first, second}})
	s.listener = listenLoopback(t)
	err := s.Start(contextFromHost(newFakeHost()))
	var duplicate *DuplicateServiceError
	if !errors.As(err, &duplicate) || duplicate.Service != echoServiceName ||
		duplicate.First.Plugin != "echo" || duplicate.Second.Plugin != "echo-copy" {
		t.Fatalf("Start error = %v, want DuplicateServiceError naming both contributors", err)
	}
	if s.Addr() != "" {
		t.Fatal("a failed assembly must not commit a listener")
	}
	_ = s.listener.Close()
}

func TestInvalidRegistrationsFailAssemblyWithAnError(t *testing.T) {
	type other interface{ Other() }
	cases := map[string]func(*Registrar){
		"nil desc": func(r *Registrar) { r.RegisterService(nil, &echoImpl{}) },
		"nil impl": func(r *Registrar) { r.RegisterService(echoDesc(), nil) },
		"wrong type": func(r *Registrar) {
			r.RegisterService(&grpc.ServiceDesc{ServiceName: "x.Y", HandlerType: (*other)(nil)}, &echoImpl{})
		},
		"empty name": func(r *Registrar) { d := echoDesc(); d.ServiceName = ""; r.RegisterService(d, &echoImpl{}) },
		"dup method": func(r *Registrar) {
			d := echoDesc()
			d.Streams[0].StreamName = "Echo"
			r.RegisterService(d, &echoImpl{})
		},
		"nil handler": func(r *Registrar) { d := echoDesc(); d.Methods[0].Handler = nil; r.RegisterService(d, &echoImpl{}) },
		"twice by self": func(r *Registrar) {
			r.RegisterService(echoDesc(), &echoImpl{})
			r.RegisterService(echoDesc(), &echoImpl{})
		},
	}
	for name, register := range cases {
		t.Run(name, func(t *testing.T) {
			contributor := plugin.Entry[ServiceRegistrar]{
				Identity: plugin.Identity{Plugin: "bad"},
				Value:    registrarFunc(func(r *Registrar) error { register(r); return nil }),
			}
			s := newServer(DefaultConfig(), contributions{registrars: []plugin.Entry[ServiceRegistrar]{contributor}})
			if err := s.Preflight(contextFromHost(newFakeHost())); err == nil {
				t.Fatal("Preflight accepted an invalid registration")
			}
		})
	}
}

func TestRegisterServicesErrorIsAttributedToTheContributor(t *testing.T) {
	contributor := plugin.Entry[ServiceRegistrar]{
		Identity: plugin.Identity{Plugin: "broken"},
		Value:    registrarFunc(func(*Registrar) error { return errors.New("no database") }),
	}
	s := newServer(DefaultConfig(), contributions{registrars: []plugin.Entry[ServiceRegistrar]{contributor}})
	err := s.Preflight(contextFromHost(newFakeHost()))
	if err == nil || !strings.Contains(err.Error(), "broken") || !strings.Contains(err.Error(), "no database") {
		t.Fatalf("Preflight error = %v", err)
	}
}

func TestRegistrarRetainedPastRegisterServicesPanics(t *testing.T) {
	var retained *Registrar
	contributor := plugin.Entry[ServiceRegistrar]{
		Identity: plugin.Identity{Plugin: "late"},
		Value:    registrarFunc(func(r *Registrar) error { retained = r; return nil }),
	}
	if _, err := collectServices([]plugin.Entry[ServiceRegistrar]{contributor}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("registering through a retained Registrar did not panic")
		}
	}()
	retained.RegisterService(echoDesc(), &echoImpl{})
}

func TestListenerFailureFailsStartWithoutSubmittingATask(t *testing.T) {
	occupied := listenLoopback(t)
	defer occupied.Close()
	cfg := DefaultConfig()
	cfg.Addr = occupied.Addr().String()
	s := newServer(cfg, contributions{registrars: []plugin.Entry[ServiceRegistrar]{echoContributor(&echoImpl{})}})
	host := newFakeHost()
	err := s.Start(contextFromHost(host))
	if err == nil || !strings.Contains(err.Error(), "failed to listen on") {
		t.Fatalf("Start error = %v, want a listen failure", err)
	}
	if host.submitted() != 0 {
		t.Fatal("a failed listen must not submit a serving task")
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after failed Start = %v", err)
	}
}

func TestRejectedServingTaskClosesTheListener(t *testing.T) {
	s := newServer(DefaultConfig(), contributions{})
	s.listener = listenLoopback(t)
	addr := s.listener.Addr().String()
	host := newFakeHost()
	host.rejectTasks()
	if err := s.Start(contextFromHost(host)); err == nil {
		t.Fatal("Start succeeded although its serving task was rejected")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listener still bound after a rejected task: %v", err)
	}
	_ = ln.Close()
}

func TestPreflightBindsNothingAndReportsTheMethodTable(t *testing.T) {
	occupied := listenLoopback(t)
	defer occupied.Close()
	cfg := DefaultConfig()
	// An address already in use: Preflight must not try to bind it.
	cfg.Addr = occupied.Addr().String()
	s := newServer(cfg, contributions{registrars: []plugin.Entry[ServiceRegistrar]{echoContributor(&echoImpl{})}})
	host := newFakeHost()
	if err := s.Preflight(contextFromHost(host)); err != nil {
		t.Fatalf("Preflight = %v", err)
	}
	if host.submitted() != 0 || s.Addr() != "" || s.server != nil {
		t.Fatal("Preflight activated something")
	}

	a, err := s.assemble(contextFromHost(host))
	if err != nil {
		t.Fatal(err)
	}
	table := renderMethodTable(a.services)
	for _, want := range []string{"1 services, 2 methods", "/xbc.test.Echo/Echo", "unary", "/xbc.test.Echo/Repeat", "server-stream", "echo"} {
		if !strings.Contains(table, want) {
			t.Fatalf("method table missing %q:\n%s", want, table)
		}
	}
	if line := renderListener(cfg.Addr, ""); !strings.Contains(line, "not bound") {
		t.Fatalf("validate report must say the listener is not bound: %s", line)
	}
}

func TestTrafficWaitsForTheGate(t *testing.T) {
	s := newServer(DefaultConfig(), contributions{registrars: []plugin.Entry[ServiceRegistrar]{echoContributor(&echoImpl{})}})
	s.listener = listenLoopback(t)
	host := newFakeHost()
	ctx := contextFromHost(host)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = s.Stop(context.Background())
		host.shutdown()
	}()

	conn, err := grpc.NewClient("passthrough:///"+s.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	callCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := conn.Invoke(callCtx, "/"+echoServiceName+"/Echo", wrapperspb.String("early"), new(wrapperspb.StringValue)); err == nil {
		t.Fatal("an RPC was answered before the traffic gate opened")
	}
	if err := s.OpenTraffic(ctx); err != nil {
		t.Fatal(err)
	}
	host.releaseTraffic()
	if out, err := callEcho(t, conn, "late"); err != nil || out.GetValue() != "echo:late" {
		t.Fatalf("Echo after gate = %v, %v", out, err)
	}
}

func TestStopBeforeTrafficOnlyClosesTheListener(t *testing.T) {
	s := newServer(DefaultConfig(), contributions{})
	s.listener = listenLoopback(t)
	addr := s.listener.Addr().String()
	host := newFakeHost()
	if err := s.Start(contextFromHost(host)); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	host.shutdown()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listener still bound after Stop: %v", err)
	}
	_ = ln.Close()
}

func TestGracefulStopLetsInFlightRPCsFinish(t *testing.T) {
	impl := blockingEcho()
	s := newServer(DefaultConfig(), contributions{registrars: []plugin.Entry[ServiceRegistrar]{echoContributor(impl)}})
	_, conn := startServing(t, s)

	result := make(chan error, 1)
	go func() {
		_, err := callEcho(t, conn, "slow")
		result <- err
	}()
	<-impl.entered

	stopped := make(chan error, 1)
	go func() { stopped <- s.Stop(context.Background()) }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned %v while an RPC was still in flight", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(impl.release)
	if err := <-result; err != nil {
		t.Fatalf("in-flight RPC failed during graceful stop: %v", err)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("Stop = %v", err)
	}
}

func TestStopCancelsRemainingRPCsWhenTheDeadlineEnds(t *testing.T) {
	impl := blockingEcho()
	defer close(impl.release)
	s := newServer(DefaultConfig(), contributions{registrars: []plugin.Entry[ServiceRegistrar]{echoContributor(impl)}})
	_, conn := startServing(t, s)

	result := make(chan error, 1)
	go func() {
		_, err := callEcho(t, conn, "stuck")
		result <- err
	}()
	<-impl.entered

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := s.Stop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop = %v, want a deadline error", err)
	}
	if err := <-result; status.Code(err) != codes.Unavailable {
		t.Fatalf("cancelled RPC error = %v, want codes.Unavailable", err)
	}
}
