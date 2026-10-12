package composition_test

// This file holds the Web+gRPC dual-stack cases for the gRPC transport
// (spec 2026-10-10-grpc-transport, section 3.3). Unlike the hand-driven
// assembly used by the rest of this module, these cases compose both
// transports in one process through the public facade -- xbc.New plus
// Execute -- because the properties under test are properties of the real
// runtime: one traffic gate for the whole process, readiness withdrawing
// before any drain, and shared integrations released after the services that
// depend on them. None of those can be observed through an assembly the test
// drives itself.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/xbcio/xbc"
	jwtcore "github.com/xbcio/xbc/extensions/authentication/jwt"
	"github.com/xbcio/xbc/plugin"
	grpctransport "github.com/xbcio/xbc/transport/grpc"
	grpcjwt "github.com/xbcio/xbc/transport/grpc/extensions/authentication/jwt"
	grpchealth "github.com/xbcio/xbc/transport/grpc/extensions/reliability/health"
	"github.com/xbcio/xbc/transport/web"
	ginengine "github.com/xbcio/xbc/transport/web/engines/gin"
	webjwt "github.com/xbcio/xbc/transport/web/extensions/authentication/jwt"
	webhealth "github.com/xbcio/xbc/transport/web/extensions/reliability/health"
)

const (
	// dualSecret is the one JWT secret the process is configured with.
	// Signing one token from it and presenting that same token to both
	// transports is the test's behavioral proof that both credential paths
	// resolved through the one neutral authenticator rather than through two
	// parallel identity sources.
	dualSecret = "composition-dualstack-test-secret-32-bytes"

	dualProbeService = "composition.dualstack.Probe"
	dualWhoamiMethod = "/" + dualProbeService + "/Whoami"
	dualSlowService  = "composition.dualstack.Slow"
	dualSlowMethod   = "/" + dualSlowService + "/Wait"

	dualPreDrain = time.Second
)

// dualConfigHeader is the configuration every dual-stack application in this
// file shares: the process logger is silenced, and the one plugins.jwt
// section both transports' extractors follow.
const dualConfigHeader = `xbc:
  shutdown_timeout: 15s
log:
  console:
    enabled: false
  file:
    enabled: false
plugins:
  jwt:
    secret: ` + dualSecret + `
`

func dualWebSection(addr string, preDrain time.Duration) string {
	return fmt.Sprintf("web:\n  addr: %q\n  shutdown:\n    pre_drain_delay: %s\n", addr, preDrain)
}

func dualGRPCSection(addr string, preDrain time.Duration) string {
	return fmt.Sprintf("grpc:\n  addr: %q\n  shutdown:\n    pre_drain_delay: %s\n", addr, preDrain)
}

// dualReserveAddr returns a loopback address nothing is listening on. The
// public facade hands no bound address back to its caller, so the test
// reserves one and configures it, the same pattern the Web shutdown test in
// examples/ uses.
func dualReserveAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())
	return addr
}

type dualAppResult struct {
	code int
	err  error
}

type dualAppRun struct {
	cancel context.CancelFunc
	done   chan dualAppResult
	once   sync.Once
	result dualAppResult
}

// dualBootApp starts the application through the public facade and returns a
// handle that stops it and collects the Execute outcome. Stop is idempotent:
// the cleanup registered here returns the same result a test observed.
func dualBootApp(t *testing.T, configYAML string, bundles ...plugin.Bundle) *dualAppRun {
	t.Helper()

	configPath := filepath.Join(t.TempDir(), "application.yml")
	require.NoError(t, os.WriteFile(configPath, []byte(configYAML), 0o600))

	app, err := xbc.New(xbc.WithBundles(bundles...))
	require.NoError(t, err)

	parent, cancel := context.WithCancel(context.Background())
	run := &dualAppRun{cancel: cancel, done: make(chan dualAppResult, 1)}
	go func() {
		code, executeErr := app.Execute(parent, []string{"--config", configPath})
		run.done <- dualAppResult{code: code, err: executeErr}
	}()
	t.Cleanup(func() {
		run.cancel()
		run.wait(t, 20*time.Second)
	})
	return run
}

func (r *dualAppRun) wait(t *testing.T, timeout time.Duration) dualAppResult {
	t.Helper()
	r.once.Do(func() {
		select {
		case r.result = <-r.done:
		case <-time.After(timeout):
			t.Fatal("timed out waiting for the application to return from Execute")
		}
	})
	return r.result
}

func dualWaitFor(t *testing.T, timeout time.Duration, what string, condition func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if last = condition(); last == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: %v", what, last)
}

// ── the probe plugin both transports share ─────────────────────────────

// dualProbeHandlers is the handler interface of the hand-written test
// services whose descriptors have the shape protoc-gen-go-grpc generates.
type dualProbeHandlers interface {
	Whoami(context.Context, *wrapperspb.StringValue) (*wrapperspb.StringValue, error)
	Wait(context.Context, *wrapperspb.StringValue) (*wrapperspb.StringValue, error)
}

// dualProbe is one plugin primary value that contributes to both transports:
// a Web route contributor and a gRPC service registrar for the same identity
// source. Its slow endpoints block until the test releases them, which is how
// the shutdown case holds work in flight across the stop request.
type dualProbe struct {
	slowEntered chan struct{}
	release     chan struct{}
	recorder    *dualRecorder // non-nil only in the shutdown case
}

var (
	_ web.RouteContributor           = (*dualProbe)(nil)
	_ grpctransport.ServiceRegistrar = (*dualProbe)(nil)
)

func dualNewProbe() *dualProbe {
	return &dualProbe{slowEntered: make(chan struct{}, 4), release: make(chan struct{})}
}

func (p *dualProbe) RegisterRoutes(router *web.Router) {
	router.GET("/whoami", p.handleWhoami).Name("dualstack.whoami").Auth(web.Accepts("jwt"))
	router.GET("/slow", p.handleSlow).Name("dualstack.slow").Auth(web.Public())
}

func (p *dualProbe) RegisterServices(registrar *grpctransport.Registrar) error {
	registrar.RegisterService(dualWhoamiDesc(), p)
	registrar.Methods(dualProbeService).Auth(grpctransport.Accepts("jwt"))
	registrar.RegisterService(dualSlowDesc(), p)
	registrar.Methods(dualSlowService).Auth(grpctransport.Public())
	return nil
}

func (p *dualProbe) handleWhoami(_ context.Context, c *web.Ctx) error {
	principal, _ := web.CurrentPrincipal(c)
	c.JSON(http.StatusOK, map[string]string{"subject": principal.Subject})
	return nil
}

func (p *dualProbe) handleSlow(_ context.Context, c *web.Ctx) error {
	p.slowEntered <- struct{}{}
	<-p.release
	c.JSON(http.StatusOK, map[string]string{"status": "done"})
	return nil
}

func (p *dualProbe) Whoami(ctx context.Context, _ *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
	principal, ok := grpctransport.PrincipalFromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "composition probe: no principal")
	}
	return wrapperspb.String(principal.Subject), nil
}

func (p *dualProbe) Wait(_ context.Context, _ *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
	p.slowEntered <- struct{}{}
	<-p.release
	return wrapperspb.String("done"), nil
}

func (p *dualProbe) Stop(context.Context) error {
	if p.recorder != nil {
		p.recorder.record("probe.stop")
	}
	return nil
}

func dualWhoamiDesc() *grpc.ServiceDesc {
	return &grpc.ServiceDesc{
		ServiceName: dualProbeService,
		HandlerType: (*dualProbeHandlers)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Whoami",
			Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				in := new(wrapperspb.StringValue)
				if err := dec(in); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return srv.(dualProbeHandlers).Whoami(ctx, in)
				}
				info := &grpc.UnaryServerInfo{Server: srv, FullMethod: dualWhoamiMethod}
				return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
					return srv.(dualProbeHandlers).Whoami(ctx, req.(*wrapperspb.StringValue))
				})
			},
		}},
	}
}

func dualSlowDesc() *grpc.ServiceDesc {
	return &grpc.ServiceDesc{
		ServiceName: dualSlowService,
		HandlerType: (*dualProbeHandlers)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Wait",
			Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				in := new(wrapperspb.StringValue)
				if err := dec(in); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return srv.(dualProbeHandlers).Wait(ctx, in)
				}
				info := &grpc.UnaryServerInfo{Server: srv, FullMethod: dualSlowMethod}
				return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
					return srv.(dualProbeHandlers).Wait(ctx, req.(*wrapperspb.StringValue))
				})
			},
		}},
	}
}

// dualProbeBundle packages the probe. integrationKey, when non-empty, adds an
// input edge on the shared integration plugin, which is what makes the
// integration start before and stop after every transport that uses it.
func dualProbeBundle(probe *dualProbe, integrationKey plugin.Key) plugin.Bundle {
	options := plugin.Options[*dualProbe]{
		Exports: plugin.Contracts(
			plugin.ExportAs[web.RouteContributor](func(value *dualProbe) web.RouteContributor { return value }),
			plugin.ExportAs[grpctransport.ServiceRegistrar](func(value *dualProbe) grpctransport.ServiceRegistrar { return value }),
		),
	}
	if integrationKey != "" {
		options.Inputs = plugin.Inputs(plugin.RefTo[*dualIntegration](integrationKey))
	}
	return plugin.BundleOf(plugin.Define(
		"composition-dualstack-probe",
		func(plugin.BuildContext) (*dualProbe, error) { return probe, nil },
		options,
	))
}

// ── the shared integration plugin ──────────────────────────────────────

// dualRecorder collects what the shutdown case observed, in call order, under
// one lock: the runtime drives different goroutines into these hooks.
type dualRecorder struct {
	mu sync.Mutex

	order []string
	// dialErrors are recorded during the drain phase. If either listener were
	// still open when the drain began, the dial would succeed and store the
	// error here.
	webDialErr  error
	grpcDialErr error

	webAddr  string
	grpcAddr string
}

func (r *dualRecorder) record(step string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, step)
}

func (r *dualRecorder) snapshot() (order []string, webErr, grpcErr error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...), r.webDialErr, r.grpcDialErr
}

// dualIntegration is the shared integration both transports depend on: the
// probe declares an input on it, so it is released after both transports have
// stopped. It is also a Drainer, so its Drain hook runs in the drain phase --
// after the ingress stop and before the remaining Stops -- which is where it
// records whether both listeners were already closed.
type dualIntegration struct {
	recorder *dualRecorder
}

var _ plugin.Drainer = (*dualIntegration)(nil)

func (i *dualIntegration) Drain(context.Context) error {
	i.recorder.mu.Lock()
	i.recorder.webDialErr = dualDialRefused(i.recorder.webAddr)
	i.recorder.grpcDialErr = dualDialRefused(i.recorder.grpcAddr)
	i.recorder.mu.Unlock()
	i.recorder.record("integration.drain")
	return nil
}

func (i *dualIntegration) Stop(context.Context) error {
	i.recorder.record("integration.stop")
	return nil
}

// dualAssertBound reports whether something accepts TCP connections on addr.
// It proves a listener is bound without claiming anything about what that
// listener answers.
func dualAssertBound(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

// dualDialRefused succeeds only when nothing is listening anymore: a timeout
// is a failure too, because it means something accepted the connection.
func dualDialRefused(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("a listener still accepts connections on %s during the drain phase", addr)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("dial %s: got %v, want connection refused", addr, err)
	}
	return nil
}

func dualIntegrationBundle(integration *dualIntegration) plugin.Bundle {
	return plugin.BundleOf(plugin.Define(
		"composition-dualstack-integration",
		func(plugin.BuildContext) (*dualIntegration, error) { return integration, nil },
		plugin.Options[*dualIntegration]{},
	))
}

// ── the blocked-failure plugin ─────────────────────────────────────────

// dualBlockedFailure is a plugin on one transport's startup path whose Start
// hook blocks until the test releases it and then fails. Its key sorts after
// every plugin it must start behind ("zz-..."), so the start walk reaches it
// only once the sibling transport has finished starting.
type dualBlockedFailure struct {
	entered chan struct{}
	release chan struct{}
	err     error
}

var (
	_ plugin.Runner                  = (*dualBlockedFailure)(nil)
	_ grpctransport.ServiceRegistrar = (*dualBlockedFailure)(nil)
	_ web.RouteContributor           = (*dualBlockedFailure)(nil)
)

func dualNewBlockedFailure(message string) *dualBlockedFailure {
	return &dualBlockedFailure{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		err:     errors.New(message),
	}
}

func (f *dualBlockedFailure) Start(*plugin.Context) error {
	close(f.entered)
	<-f.release
	return f.err
}

func (*dualBlockedFailure) RegisterServices(*grpctransport.Registrar) error { return nil }

func (*dualBlockedFailure) RegisterRoutes(*web.Router) {}

// dualBlockedRegistrarBundle places the failure on the gRPC startup path: the
// gRPC server collects every ServiceRegistrar, so it cannot start before this
// plugin does, while the Web stack -- which does not consume registrars --
// starts normally beside it.
func dualBlockedRegistrarBundle(failure *dualBlockedFailure) plugin.Bundle {
	return plugin.BundleOf(plugin.Define(
		"zz-composition-failed-registrar",
		func(plugin.BuildContext) (*dualBlockedFailure, error) { return failure, nil },
		plugin.Options[*dualBlockedFailure]{
			Exports: plugin.Contracts(
				plugin.ExportAs[grpctransport.ServiceRegistrar](func(value *dualBlockedFailure) grpctransport.ServiceRegistrar { return value }),
			),
		},
	))
}

// dualBlockedRoutesBundle is the mirror image: the Web server collects every
// RouteContributor, so the Web stack waits for this plugin while the gRPC
// server starts normally beside it.
func dualBlockedRoutesBundle(failure *dualBlockedFailure) plugin.Bundle {
	return plugin.BundleOf(plugin.Define(
		"zz-composition-failed-routes",
		func(plugin.BuildContext) (*dualBlockedFailure, error) { return failure, nil },
		plugin.Options[*dualBlockedFailure]{
			Exports: plugin.Contracts(
				plugin.ExportAs[web.RouteContributor](func(value *dualBlockedFailure) web.RouteContributor { return value }),
			),
		},
	))
}

func dualWaitEntered(t *testing.T, failure *dualBlockedFailure) {
	t.Helper()
	select {
	case <-failure.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the failing plugin's Start hook to run")
	}
}

// ── client helpers ─────────────────────────────────────────────────────

func dualDial(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func dualSignToken(t *testing.T, subject string) string {
	t.Helper()
	config := jwtcore.DefaultConfig()
	config.Secret = dualSecret
	authenticator, err := jwtcore.New(config)
	require.NoError(t, err)
	token, err := authenticator.Sign(subject, nil)
	require.NoError(t, err)
	return token
}

type dualHTTPResult struct {
	status int
	body   string
	err    error
}

func dualGet(addr, path, token string, timeout time.Duration) dualHTTPResult {
	client := &http.Client{Timeout: timeout, Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	request, err := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return dualHTTPResult{err: err}
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return dualHTTPResult{err: err}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	if err != nil {
		return dualHTTPResult{err: err}
	}
	return dualHTTPResult{status: response.StatusCode, body: string(body)}
}

func dualHTTPReady(addr string) error {
	result := dualGet(addr, "/readyz", "", 500*time.Millisecond)
	if result.err != nil {
		return result.err
	}
	if result.status != http.StatusOK {
		return fmt.Errorf("readyz status = %d", result.status)
	}
	return nil
}

func dualHealth(conn *grpc.ClientConn, want healthpb.HealthCheckResponse_ServingStatus) error {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	response, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		return err
	}
	if response.GetStatus() != want {
		return fmt.Errorf("health status = %s, want %s", response.GetStatus(), want)
	}
	return nil
}

func dualWhoami(conn *grpc.ClientConn, token string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	}
	out := new(wrapperspb.StringValue)
	if err := conn.Invoke(ctx, dualWhoamiMethod, wrapperspb.String(""), out); err != nil {
		return "", err
	}
	return out.GetValue(), nil
}

func dualSubject(t *testing.T, result dualHTTPResult) string {
	t.Helper()
	var body map[string]string
	require.NoError(t, json.Unmarshal([]byte(result.body), &body))
	return body["subject"]
}

// ── traffic samplers for the failed-startup case ───────────────────────

// dualSampler records whether a transport ever answered a probe. "Answered"
// is the whole criterion: any response, of any status, proves the traffic
// gate opened for that transport. A listener that is bound but still behind
// the closed gate accepts connections at the kernel and never answers, so
// every attempt there ends in a client-side timeout, not in a response.
type dualSampler struct {
	stop   chan struct{}
	done   chan struct{}
	served atomic.Bool
}

func (s *dualSampler) finish(t *testing.T, what string) {
	t.Helper()
	close(s.stop)
	<-s.done
	require.False(t, s.served.Load(),
		"%s answered a probe even though the other transport failed to start; a failed composition must open no traffic anywhere", what)
}

func dualSampleHTTP(addr string) *dualSampler {
	sampler := &dualSampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(sampler.done)
		for {
			select {
			case <-sampler.stop:
				return
			default:
			}
			result := dualGet(addr, "/readyz", "", 150*time.Millisecond)
			if result.err == nil {
				sampler.served.Store(true)
				return
			}
		}
	}()
	return sampler
}

func dualSampleGRPC(addr string) *dualSampler {
	sampler := &dualSampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(sampler.done)
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			select {
			case <-sampler.stop:
				return
			default:
			}
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			_, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
			cancel()
			if err == nil {
				sampler.served.Store(true)
				return
			}
		}
	}()
	return sampler
}

// ── the cases ──────────────────────────────────────────────────────────

// TestDualStackComposesBothTransportsWithOnePrincipal composes the Web and
// gRPC stacks in one process. One token, signed once from the one configured
// secret, authenticates on both transports and resolves to the same subject,
// and each transport independently refuses the anonymous caller: there is no
// parallel identity source, and neither transport's authentication is
// decorative.
func TestDualStackComposesBothTransportsWithOnePrincipal(t *testing.T) {
	webAddr := dualReserveAddr(t)
	grpcAddr := dualReserveAddr(t)
	probe := dualNewProbe()

	run := dualBootApp(t,
		dualConfigHeader+dualWebSection(webAddr, 0)+dualGRPCSection(grpcAddr, 0),
		web.Bundle(), ginengine.Bundle(), webjwt.Bundle(), webhealth.Bundle(),
		grpctransport.Bundle(), grpcjwt.Bundle(), grpchealth.Bundle(),
		dualProbeBundle(probe, ""),
	)
	conn := dualDial(t, grpcAddr)

	dualWaitFor(t, 10*time.Second, "both transports to serve", func() error {
		if err := dualHTTPReady(webAddr); err != nil {
			return err
		}
		return dualHealth(conn, healthpb.HealthCheckResponse_SERVING)
	})

	t.Run("both transports refuse the anonymous caller", func(t *testing.T) {
		anonymous := dualGet(webAddr, "/whoami", "", 3*time.Second)
		require.NoError(t, anonymous.err)
		require.Equal(t, http.StatusUnauthorized, anonymous.status)

		if _, err := dualWhoami(conn, ""); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("anonymous gRPC call = %v, want Unauthenticated", err)
		}
	})

	t.Run("one token resolves to one principal on both transports", func(t *testing.T) {
		token := dualSignToken(t, "alice")

		httpResult := dualGet(webAddr, "/whoami", token, 3*time.Second)
		require.NoError(t, httpResult.err)
		require.Equal(t, http.StatusOK, httpResult.status)
		require.Equal(t, "alice", dualSubject(t, httpResult))

		subject, err := dualWhoami(conn, token)
		require.NoError(t, err)
		require.Equal(t, "alice", subject,
			"the same token must resolve to the same subject on both transports")
	})

	run.cancel()
	outcome := run.wait(t, 20*time.Second)
	require.NoError(t, outcome.err)
	require.Equal(t, 0, outcome.code)
}

// TestEachTransportIsSelectableAlone composes each transport without the
// other in a process of its own. A service that selects only one transport
// must boot, serve, and shut down without ever naming the sibling stack.
func TestEachTransportIsSelectableAlone(t *testing.T) {
	t.Run("web alone", func(t *testing.T) {
		addr := dualReserveAddr(t)
		probe := dualNewProbe()
		run := dualBootApp(t, dualConfigHeader+dualWebSection(addr, 0),
			web.Bundle(), ginengine.Bundle(), webjwt.Bundle(), webhealth.Bundle(),
			dualProbeBundle(probe, ""),
		)

		dualWaitFor(t, 10*time.Second, "the Web transport to serve", func() error { return dualHTTPReady(addr) })

		result := dualGet(addr, "/whoami", dualSignToken(t, "alice"), 3*time.Second)
		require.NoError(t, result.err)
		require.Equal(t, http.StatusOK, result.status)
		require.Equal(t, "alice", dualSubject(t, result))

		run.cancel()
		outcome := run.wait(t, 20*time.Second)
		require.NoError(t, outcome.err)
		require.Equal(t, 0, outcome.code)
	})

	t.Run("grpc alone", func(t *testing.T) {
		addr := dualReserveAddr(t)
		probe := dualNewProbe()
		run := dualBootApp(t, dualConfigHeader+dualGRPCSection(addr, 0),
			grpctransport.Bundle(), grpcjwt.Bundle(), grpchealth.Bundle(),
			dualProbeBundle(probe, ""),
		)
		conn := dualDial(t, addr)

		dualWaitFor(t, 10*time.Second, "the gRPC transport to serve", func() error {
			return dualHealth(conn, healthpb.HealthCheckResponse_SERVING)
		})

		subject, err := dualWhoami(conn, dualSignToken(t, "alice"))
		require.NoError(t, err)
		require.Equal(t, "alice", subject)

		run.cancel()
		outcome := run.wait(t, 20*time.Second)
		require.NoError(t, outcome.err)
		require.Equal(t, 0, outcome.code)
	})
}

// TestGRPCShutdownHoldsThePreDrainReadinessWindow pins
// grpc.shutdown.pre_drain_delay on the gRPC stack alone: after a stop request,
// readiness withdraws -- the health service answers NOT_SERVING through an
// answered RPC, not an Unavailable connection failure -- while the listener
// keeps accepting fresh probes for the configured window, and the listener
// closes once the window ends. The dual-stack shutdown case cannot guard this
// delay: the Web server's own pre-drain runs first in the unwind order and
// keeps the gRPC listener answering long enough that a gRPC server skipping
// its window would still pass there.
func TestGRPCShutdownHoldsThePreDrainReadinessWindow(t *testing.T) {
	addr := dualReserveAddr(t)
	run := dualBootApp(t, dualConfigHeader+dualGRPCSection(addr, dualPreDrain),
		grpctransport.Bundle(), grpcjwt.Bundle(), grpchealth.Bundle(),
	)
	conn := dualDial(t, addr)

	dualWaitFor(t, 10*time.Second, "the gRPC transport to serve", func() error {
		return dualHealth(conn, healthpb.HealthCheckResponse_SERVING)
	})

	run.cancel()

	// Give the unwinding walk time to reach and invoke the server's Stop. From
	// here a server without the pre-drain window has already closed its
	// listener, so the NOT_SERVING below can only be observed through the
	// deliberately held-open window, not by racing the walk.
	time.Sleep(300 * time.Millisecond)
	dualWaitFor(t, 5*time.Second, "gRPC health to withdraw through the pre-drain window", func() error {
		return dualHealth(conn, healthpb.HealthCheckResponse_NOT_SERVING)
	})

	outcome := run.wait(t, 20*time.Second)
	require.NoError(t, outcome.err)
	require.Equal(t, 0, outcome.code)
	require.NoError(t, dualDialRefused(addr), "the gRPC listener must be closed once the pre-drain window ends")
}

// TestStartupFailureLeavesBothTransportsWithoutTraffic fails one transport's
// startup while the other transport has already started and bound its
// listener. The blocked-failure plugin makes the window deterministic: its
// Start hook blocks until the test releases it, and because it belongs to one
// transport's startup path, the sibling transport -- which does not depend on
// it -- has finished starting by then. The bound sibling must answer nothing,
// because the process-wide traffic gate opens only after every transport has
// started. After the failed startup returns, its port is closed again too.
//
// A raw port conflict would exercise the same gate, but it fails before
// either transport reaches its listener in this composition's start order,
// which would leave the interesting window ("bound, but behind a closed
// gate") unobserved.
func TestStartupFailureLeavesBothTransportsWithoutTraffic(t *testing.T) {
	t.Run("the gRPC stack fails while the Web service is already bound", func(t *testing.T) {
		webAddr := dualReserveAddr(t)
		grpcAddr := dualReserveAddr(t)
		failure := dualNewBlockedFailure("the gRPC stack fails to start")

		run := dualBootApp(t,
			dualConfigHeader+dualWebSection(webAddr, 0)+dualGRPCSection(grpcAddr, 0),
			web.Bundle(), ginengine.Bundle(), webjwt.Bundle(), webhealth.Bundle(),
			grpctransport.Bundle(), grpcjwt.Bundle(), grpchealth.Bundle(),
			dualBlockedRegistrarBundle(failure),
			dualProbeBundle(dualNewProbe(), ""),
		)

		dualWaitEntered(t, failure)
		dualWaitFor(t, 5*time.Second, "the Web listener to be bound", func() error { return dualAssertBound(webAddr) })

		// The listener accepts connections, and still nothing answers: a bound
		// listener behind a closed gate is not serving.
		require.Error(t, dualGet(webAddr, "/readyz", "", 250*time.Millisecond).err,
			"a bound listener behind the closed traffic gate must not answer")
		sampler := dualSampleHTTP(webAddr)

		close(failure.release)
		outcome := run.wait(t, 15*time.Second)
		sampler.finish(t, "the Web transport")

		require.Error(t, outcome.err, "a failing transport start must fail the startup")
		require.NotEqual(t, 0, outcome.code)
		require.NoError(t, dualDialRefused(webAddr), "the Web listener must be closed after the failed startup")
	})

	t.Run("the Web stack fails while the gRPC service is already bound", func(t *testing.T) {
		webAddr := dualReserveAddr(t)
		grpcAddr := dualReserveAddr(t)
		failure := dualNewBlockedFailure("the Web stack fails to start")

		run := dualBootApp(t,
			dualConfigHeader+dualWebSection(webAddr, 0)+dualGRPCSection(grpcAddr, 0),
			web.Bundle(), ginengine.Bundle(), webjwt.Bundle(), webhealth.Bundle(),
			grpctransport.Bundle(), grpcjwt.Bundle(), grpchealth.Bundle(),
			dualBlockedRoutesBundle(failure),
			dualProbeBundle(dualNewProbe(), ""),
		)

		dualWaitEntered(t, failure)
		dualWaitFor(t, 5*time.Second, "the gRPC listener to be bound", func() error { return dualAssertBound(grpcAddr) })

		// The gRPC listener accepts the connection and answers no RPC while the
		// gate is closed.
		require.Error(t, dualHealth(dualDial(t, grpcAddr), healthpb.HealthCheckResponse_SERVING),
			"a bound gRPC listener behind the closed traffic gate must not answer")
		sampler := dualSampleGRPC(grpcAddr)

		close(failure.release)
		outcome := run.wait(t, 15*time.Second)
		sampler.finish(t, "the gRPC transport")

		require.Error(t, outcome.err, "a failing transport start must fail the startup")
		require.NotEqual(t, 0, outcome.code)
		require.NoError(t, dualDialRefused(grpcAddr), "the gRPC listener must be closed after the failed startup")
	})
}

// TestShutdownWithdrawsReadinessBeforeDrainingAndReleasesSharedLast pins the
// shutdown order on the real runtime. With one request in flight on each
// transport, a stop request must:
//
//  1. withdraw readiness on both transports -- 503 on /readyz and
//     NOT_SERVING on the health service -- while both listeners still accept
//     fresh probes;
//  2. let each transport drain: both in-flight requests finish successfully
//     after readiness is down;
//  3. run the shared integration's drain phase only after both listeners are
//     closed, and release the integration after the probe that depends on it.
func TestShutdownWithdrawsReadinessBeforeDrainingAndReleasesSharedLast(t *testing.T) {
	webAddr := dualReserveAddr(t)
	grpcAddr := dualReserveAddr(t)
	recorder := &dualRecorder{webAddr: webAddr, grpcAddr: grpcAddr}
	integration := &dualIntegration{recorder: recorder}
	probe := dualNewProbe()
	probe.recorder = recorder

	run := dualBootApp(t,
		dualConfigHeader+dualWebSection(webAddr, dualPreDrain)+dualGRPCSection(grpcAddr, dualPreDrain),
		web.Bundle(), ginengine.Bundle(), webjwt.Bundle(), webhealth.Bundle(),
		grpctransport.Bundle(), grpcjwt.Bundle(), grpchealth.Bundle(),
		dualIntegrationBundle(integration),
		dualProbeBundle(probe, "composition-dualstack-integration"),
	)
	conn := dualDial(t, grpcAddr)

	dualWaitFor(t, 10*time.Second, "both transports to serve", func() error {
		if err := dualHTTPReady(webAddr); err != nil {
			return err
		}
		return dualHealth(conn, healthpb.HealthCheckResponse_SERVING)
	})

	httpResult := make(chan dualHTTPResult, 1)
	go func() { httpResult <- dualGet(webAddr, "/slow", "", 20*time.Second) }()
	grpcResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out := new(wrapperspb.StringValue)
		grpcResult <- conn.Invoke(ctx, dualSlowMethod, wrapperspb.String(""), out)
	}()

	for range 2 {
		select {
		case <-probe.slowEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for both in-flight requests to enter their handlers")
		}
	}

	run.cancel()

	dualWaitFor(t, 5*time.Second, "gRPC health to withdraw", func() error {
		return dualHealth(conn, healthpb.HealthCheckResponse_NOT_SERVING)
	})

	// Give the unwinding walk time to reach the Web server's Stop. The 503
	// below must be observable because the listener is deliberately held open
	// through the pre-drain window, not because the test happened to ask
	// before the unwinding walk got there; without this pause, a server that
	// closed its listener immediately would still pass the check by racing it.
	time.Sleep(300 * time.Millisecond)
	dualWaitFor(t, 5*time.Second, "Web readiness to withdraw", func() error {
		result := dualGet(webAddr, "/readyz", "", 500*time.Millisecond)
		if result.err != nil {
			return result.err
		}
		if result.status != http.StatusServiceUnavailable {
			return fmt.Errorf("readyz status = %d, want 503", result.status)
		}
		return nil
	})

	close(probe.release)

	select {
	case result := <-httpResult:
		require.NoError(t, result.err)
		require.Equal(t, http.StatusOK, result.status, "the Web drain must finish work accepted before the stop request")
	case <-time.After(10 * time.Second):
		t.Fatal("the accepted Web request did not finish during the drain")
	}
	select {
	case err := <-grpcResult:
		require.NoError(t, err, "the gRPC drain must finish work accepted before the stop request")
	case <-time.After(10 * time.Second):
		t.Fatal("the accepted gRPC call did not finish during the drain")
	}

	outcome := run.wait(t, 20*time.Second)
	require.NoError(t, outcome.err)
	require.Equal(t, 0, outcome.code)

	order, webDialErr, grpcDialErr := recorder.snapshot()
	require.NoError(t, webDialErr, "the Web listener must already be closed when the drain phase begins")
	require.NoError(t, grpcDialErr, "the gRPC listener must already be closed when the drain phase begins")
	require.Contains(t, order, "integration.drain")
	require.Contains(t, order, "probe.stop")
	require.Contains(t, order, "integration.stop")
	drainIndex := dualIndexOf(order, "integration.drain")
	probeIndex := dualIndexOf(order, "probe.stop")
	integrationIndex := dualIndexOf(order, "integration.stop")
	require.Less(t, drainIndex, integrationIndex, "the shared integration must drain before it is released")
	require.Less(t, probeIndex, integrationIndex,
		"the shared integration must be released after the probe that depends on it")
}

func dualIndexOf(values []string, want string) int {
	for index, value := range values {
		if value == want {
			return index
		}
	}
	return -1
}
