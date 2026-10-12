package examples_test

// This file covers the gRPC management plane through the public composition
// path, the way a gRPC-only deployment composes it. Three properties are only
// observable from a booted process: which of the two endpoints a given
// selection puts on the listener, that a management address this process must
// not expose is refused before anything starts, and that the operator listener
// is still answering while a shutdown drains the serving plane -- the window
// the Web management listener already publishes, restated here because the two
// planes share the policy rather than the code.
//
// The package tests under transport/grpc/extensions/management cover the
// pieces: the configuration rules, the routes, the drain of an in-flight
// scrape. What none of them can build is a real composition whose serving
// transport is mid-drain, which is what the last test arranges -- twice over,
// since the operator listener has to outlive both the serving drain and the
// drain phase that follows it.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/plugin"
	grpctransport "github.com/xbcio/xbc/transport/grpc"
	grpcmanagement "github.com/xbcio/xbc/transport/grpc/extensions/management"
	grpcmetrics "github.com/xbcio/xbc/transport/grpc/extensions/observability/metrics"
	grpchealth "github.com/xbcio/xbc/transport/grpc/extensions/reliability/health"
)

// heldCallService is one unary method that reports its arrival and then holds
// the call until the test releases it or its context ends. A hand-written
// descriptor is enough -- the server sees exactly what a generated
// registration hands it, and a parked handler is the only state in which a
// graceful stop is observably under way.
type heldCallService struct {
	entered chan struct{}
	release chan struct{}
}

func newHeldCallService() *heldCallService {
	return &heldCallService{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
}

const heldCallServiceName = "xbc.test.Held"

type heldCallServer interface {
	Hold(context.Context, *wrapperspb.StringValue) (*wrapperspb.StringValue, error)
}

func heldCallDesc() *grpc.ServiceDesc {
	return &grpc.ServiceDesc{
		ServiceName: heldCallServiceName,
		HandlerType: (*heldCallServer)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Hold",
			Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				in := new(wrapperspb.StringValue)
				if err := dec(in); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return srv.(heldCallServer).Hold(ctx, in)
				}
				info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + heldCallServiceName + "/Hold"}
				return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
					return srv.(heldCallServer).Hold(ctx, req.(*wrapperspb.StringValue))
				})
			},
		}},
	}
}

func (service *heldCallService) RegisterServices(r *grpctransport.Registrar) error {
	r.RegisterService(heldCallDesc(), service)
	// Public beside its registration: the composition has no authenticator,
	// and every method is denied by default.
	r.Methods(heldCallServiceName).Auth(grpctransport.Public())
	return nil
}

func (service *heldCallService) Hold(ctx context.Context, in *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
	select {
	case service.entered <- struct{}{}:
	default:
	}
	var err error
	select {
	case <-service.release:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return wrapperspb.String("held:" + in.GetValue()), nil
}

func heldCallBundle(service *heldCallService) plugin.Bundle {
	return plugin.BundleOf(plugin.Define("held-call",
		func(plugin.BuildContext) (*heldCallService, error) { return service, nil },
		plugin.Options[*heldCallService]{Exports: plugin.Contracts(
			plugin.ExportAs[grpctransport.ServiceRegistrar](func(service *heldCallService) grpctransport.ServiceRegistrar { return service }),
		)},
	))
}

type grpcManagementResult struct {
	code int
	err  error
}

// heldDrainProbe is a plugin that accepts no work of its own and holds the
// drain phase open until the test releases it. A composition that has one is
// the only way to observe what is still reachable once the serving plane has
// finished draining and the remaining Stops have not yet run.
type heldDrainProbe struct {
	entered chan struct{}
	release chan struct{}
}

func newHeldDrainProbe() *heldDrainProbe {
	return &heldDrainProbe{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

var _ plugin.Drainer = (*heldDrainProbe)(nil)

func (probe *heldDrainProbe) Drain(ctx context.Context) error {
	select {
	case probe.entered <- struct{}{}:
	default:
	}
	select {
	case <-probe.release:
	case <-ctx.Done():
	}
	return nil
}

func (*heldDrainProbe) Stop(context.Context) error { return nil }

func heldDrainProbeBundle(probe *heldDrainProbe) plugin.Bundle {
	return plugin.BundleOf(plugin.Define("held-drain",
		func(plugin.BuildContext) (*heldDrainProbe, error) { return probe, nil },
		plugin.Options[*heldDrainProbe]{},
	))
}

// guardedClose returns an idempotent close over a test channel and registers it
// as cleanup, so an assertion that fails mid-test cannot leave a hold the
// process is waiting on.
func guardedClose(t *testing.T, ch chan struct{}) func() {
	t.Helper()
	closed := false
	release := func() {
		if !closed {
			closed = true
			close(ch)
		}
	}
	t.Cleanup(release)
	return release
}

// scrapeDuringShutdown fetches the scrape endpoint over a connection this call
// opens, and reports what phase it was in when it failed. A fresh connection is
// the point: the property is that the listener still accepts one, not that an
// earlier keep-alive happened to survive.
func scrapeDuringShutdown(t *testing.T, url, phase string) string {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	response, err := client.Get(url)
	require.NoErrorf(t, err, "the management listener must still accept a scrape during %s", phase)
	body, readErr := io.ReadAll(response.Body)
	require.NoError(t, readErr)
	require.NoError(t, response.Body.Close())
	require.Equalf(t, http.StatusOK, response.StatusCode, "scrape during %s", phase)
	return string(body)
}

// startGRPCManagementApp writes config, boots it through the public Execute
// path beside bundles, and returns the cancel that begins shutdown and
// Execute's result channel. It does not wait for readiness: what "ready" means
// depends on what the composition selected, so each caller waits on the
// listener it is about to assert against.
func startGRPCManagementApp(t *testing.T, config string, bundles ...plugin.Bundle) (context.CancelFunc, <-chan grpcManagementResult) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "application.yml")
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0o600))

	app, err := xbc.New(xbc.WithBundles(bundles...))
	require.NoError(t, err)

	parent, cancel := context.WithCancel(context.Background())
	done := make(chan grpcManagementResult, 1)
	go func() {
		code, executeErr := app.Execute(parent, []string{"--config", configPath})
		done <- grpcManagementResult{code: code, err: executeErr}
	}()
	t.Cleanup(cancel)
	return cancel, done
}

// awaitManagement polls until the management listener produces any HTTP
// response at all, so an assertion compares the status the composition chose
// rather than the paucity of its startup. A refused connection means "not
// listening yet", not "no route", and a process that exits first is reported
// as that rather than as an endpoint that never answered.
func awaitManagement(t *testing.T, done <-chan grpcManagementResult, method, url string) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: 250 * time.Millisecond}
	t.Cleanup(client.CloseIdleConnections)
	deadline := time.Now().Add(probeWait)
	for {
		request, err := http.NewRequest(method, url, nil)
		require.NoError(t, err)
		response, err := client.Do(request)
		if err == nil {
			return response
		}
		select {
		case result := <-done:
			t.Fatalf("the process exited before %s %s answered: code=%d error=%v", method, url, result.code, result.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s %s: %v", method, url, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// managementProbe is one path the operator listener must answer, with the
// substring its body must carry where the endpoint has a body to check.
type managementProbe struct {
	path     string
	status   int
	contains string
}

// TestGRPCManagementAnswersOnlyTheEndpointsItsSourcesSelect pins both
// directions of the plane's conditional routing from a booted composition: the
// scrape endpoint exists only once the neutral metrics plugin is selected, and
// the profiling subtree only under its own opt-in. The endpoints otherwise
// answer 404 -- absent, not empty -- and the case that selects neither
// endpoint exists so the 404s cannot pass for a listener that failed to start.
func TestGRPCManagementAnswersOnlyTheEndpointsItsSourcesSelect(t *testing.T) {
	for _, tc := range []struct {
		name    string
		bundles []plugin.Bundle
		// section is the plugins block with the management address filled in.
		section func(addr string) string
		// probes are checked in order, and the first one doubles as the
		// readiness wait: its status is what proves the boot crossed the
		// traffic gate.
		probes []managementProbe
		// healthCheck calls grpc.health.v1.Health/Check before probing, so the
		// scrape has a real gRPC call to report.
		healthCheck bool
	}{
		{
			name: "metrics and health selected",
			bundles: []plugin.Bundle{
				grpctransport.Bundle(),
				grpchealth.Bundle(),
				grpcmetrics.Bundle(),
				grpcmanagement.Bundle(),
			},
			section: func(addr string) string {
				return fmt.Sprintf("  metrics: {}\n  grpc-management:\n    addr: %q\n", addr)
			},
			probes: []managementProbe{
				// The instrumentation is the neutral registry's: the scrape
				// reports the call the health check above just made.
				{path: "/metrics", status: http.StatusOK, contains: "xbc_grpc_requests_total"},
				{path: "/debug/pprof/", status: http.StatusNotFound},
			},
			healthCheck: true,
		},
		{
			name:    "management alone with profiling",
			bundles: []plugin.Bundle{grpctransport.Bundle(), grpcmanagement.Bundle()},
			section: func(addr string) string {
				return fmt.Sprintf("  grpc-management:\n    addr: %q\n    pprof: true\n", addr)
			},
			probes: []managementProbe{
				// Without the metrics plugin there is no registry to render,
				// so the scrape endpoint is not registered at all.
				{path: "/debug/pprof/", status: http.StatusOK, contains: "goroutine"},
				{path: "/metrics", status: http.StatusNotFound},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			grpcAddr, managementAddr := freeAddress(t), freeAddress(t)
			config := fmt.Sprintf(`log:
  console:
    enabled: false
  file:
    enabled: false
grpc:
  addr: %q
plugins:
%s`, grpcAddr, tc.section(managementAddr))
			_, done := startGRPCManagementApp(t, config, tc.bundles...)

			base := "http://" + managementAddr
			ready := awaitManagement(t, done, http.MethodGet, base+tc.probes[0].path)
			require.Equal(t, tc.probes[0].status, ready.StatusCode)
			require.NoError(t, ready.Body.Close())

			if tc.healthCheck {
				conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
				require.NoError(t, err)
				t.Cleanup(func() { _ = conn.Close() })

				callCtx, cancelCall := context.WithTimeout(context.Background(), probeWait)
				defer cancelCall()
				response, err := healthpb.NewHealthClient(conn).Check(callCtx, &healthpb.HealthCheckRequest{})
				require.NoError(t, err)
				assert.Equal(t, healthpb.HealthCheckResponse_SERVING, response.GetStatus(),
					"the management listener answers only after the traffic gate released, and the gate is what the health service reports")
			}

			// The listener has answered once, so the remaining probes are
			// assertions rather than waits.
			client := &http.Client{Timeout: 250 * time.Millisecond}
			t.Cleanup(client.CloseIdleConnections)
			for _, probe := range tc.probes {
				response, err := client.Get(base + probe.path)
				require.NoError(t, err, "%s must be answered by the management listener", probe.path)
				body, readErr := io.ReadAll(response.Body)
				require.NoError(t, readErr)
				require.NoError(t, response.Body.Close())
				assert.Equal(t, probe.status, response.StatusCode, "GET %s", probe.path)
				if probe.contains != "" {
					assert.Contains(t, string(body), probe.contains, "GET %s", probe.path)
				}
			}
		})
	}
}

// TestGRPCManagementRefusesAnAddressItMustNotExpose proves the binding policy
// from the composition root: a management address that is not loopback is
// refused until the deployment consents to it, and that consent is refused
// until it names a certificate. Both refusals happen while the plan is built,
// so the process exits with the configuration error instead of binding an
// operator surface it could not protect.
func TestGRPCManagementRefusesAnAddressItMustNotExpose(t *testing.T) {
	for _, tc := range []struct {
		name    string
		section string
		// want is the rule the refusal must name.
		want string
	}{
		{
			name:    "a remote address without consent",
			section: "    addr: \"0.0.0.0:19091\"\n",
			want:    "not loopback",
		},
		{
			name:    "consent without a certificate",
			section: "    addr: \"0.0.0.0:19091\"\n    allow_remote: true\n",
			want:    "allow_remote requires tls.cert_file",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := fmt.Sprintf(`log:
  console:
    enabled: false
  file:
    enabled: false
grpc:
  addr: %q
plugins:
  grpc-management:
%s`, freeAddress(t), tc.section)
			_, done := startGRPCManagementApp(t, config, grpctransport.Bundle(), grpcmanagement.Bundle())

			select {
			case result := <-done:
				require.Error(t, result.err)
				assert.Contains(t, result.err.Error(), "grpc-management")
				assert.Contains(t, result.err.Error(), tc.want)
				assert.Equal(t, 1, result.code)
			case <-time.After(probeWait):
				t.Fatal("timed out waiting for the composition to refuse the management address")
			}
		})
	}
}

// TestGRPCManagementStaysReachableWhileTheServingPlaneDrains pins the shutdown
// window from outside the process, twice: once while the serving plane is
// draining a parked RPC, and once after it has finished, when the drain phase
// is holding the process open. The management listener must answer a scrape on
// a fresh connection in both, and stop answering only after the process is
// gone: it is stopped in the last shutdown phase, after the ingress and after
// the drainers, because it neither opens traffic nor accepts work of its own.
func TestGRPCManagementStaysReachableWhileTheServingPlaneDrains(t *testing.T) {
	service := newHeldCallService()
	drainer := newHeldDrainProbe()
	release := guardedClose(t, service.release)
	releaseDrain := guardedClose(t, drainer.release)

	grpcAddr, managementAddr := freeAddress(t), freeAddress(t)
	config := fmt.Sprintf(`xbc:
  shutdown_timeout: 30s
  pre_stop_timeout: 0s
log:
  console:
    enabled: false
  file:
    enabled: false
grpc:
  addr: %q
plugins:
  metrics: {}
  grpc-management:
    addr: %q
`, grpcAddr, managementAddr)
	cancel, done := startGRPCManagementApp(t, config,
		grpctransport.Bundle(), grpcmetrics.Bundle(), grpcmanagement.Bundle(),
		heldCallBundle(service), heldDrainProbeBundle(drainer))

	metricsURL := "http://" + managementAddr + "/metrics"
	ready := awaitManagement(t, done, http.MethodGet, metricsURL)
	require.Equal(t, http.StatusOK, ready.StatusCode)
	require.NoError(t, ready.Body.Close())

	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	callCtx, cancelCall := context.WithTimeout(context.Background(), 2*probeWait)
	defer cancelCall()
	callResult := make(chan error, 1)
	go func() {
		callResult <- conn.Invoke(callCtx, "/"+heldCallServiceName+"/Hold", wrapperspb.String("value"), new(wrapperspb.StringValue))
	}()
	select {
	case <-service.entered:
	case <-time.After(probeWait):
		t.Fatal("the parked RPC never reached the handler, so no drain can be observed mid-call")
	}

	// Cancelling the execution context is the shutdown request. The serving
	// listener closes as the graceful stop begins and stays closed while the
	// parked call finishes, which is the first window asserted below.
	cancel()
	require.True(t, servingListenerRefused(grpcAddr, probeWait),
		"the serving listener must stop accepting while the shutdown drains")
	assert.Contains(t, scrapeDuringShutdown(t, metricsURL, "the serving drain"), `xbc_grpc_requests_in_flight{`,
		"the scrape during the serving drain must report the parked call: it is what dates the window")

	release()
	select {
	case err := <-callResult:
		require.NoError(t, err, "the drain must let an accepted call finish, not cancel it")
	case <-time.After(probeWait):
		t.Fatal("the parked RPC did not return after it was released")
	}
	select {
	case <-drainer.entered:
	case <-time.After(probeWait):
		t.Fatal("the drain phase never began after the serving plane finished draining")
	}

	// The second window: the serving plane is gone and the drain phase is under
	// way, so every ingress participant has stopped and the stops that follow
	// the drain phase have not run. The operator listener is one of those, and
	// this scrape is what holds that to a lower bound rather than to a claim.
	scrapeDuringShutdown(t, metricsURL, "the drain phase")

	releaseDrain()
	select {
	case result := <-done:
		require.NoError(t, result.err)
		assert.Equal(t, 0, result.code)
	case <-time.After(probeWait):
		t.Fatal("timed out waiting for the process to exit after its drain finished")
	}

	after := &http.Client{Timeout: 250 * time.Millisecond}
	t.Cleanup(after.CloseIdleConnections)
	_, err = after.Get(metricsURL)
	assert.Error(t, err, "the management listener must not outlive the process it reports on")
}

// servingListenerRefused reports whether nothing accepts connections at addr
// any more, polling because the ingress stop is asynchronous to the cancel.
func servingListenerRefused(addr string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err != nil {
			return true
		}
		_ = conn.Close()
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
