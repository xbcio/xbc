package health

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
	grpctransport "github.com/xbcio/xbc/transport/grpc"
)

// bootHost is the smallest faithful RuntimeHost a full-transport probe test
// needs: one execution context, one traffic gate, and task admission, the same
// three facts the runtime owns.
type bootHost struct {
	execution context.Context
	cancel    context.CancelFunc
	gate      chan struct{}
	gateOnce  sync.Once
	tasks     sync.WaitGroup
}

var _ plugin.RuntimeHost = (*bootHost)(nil)

func newBootHost() *bootHost {
	execution, cancel := context.WithCancel(context.Background())
	return &bootHost{execution: execution, cancel: cancel, gate: make(chan struct{})}
}

func (h *bootHost) ExecutionContext() context.Context                               { return h.execution }
func (*bootHost) Logger() log.Logger                                                { return log.Nop() }
func (*bootHost) ProcessInstance() string                                           { return "probe-test-process" }
func (h *bootHost) TrafficGate() <-chan struct{}                                    { return h.gate }
func (*bootHost) Admission(plugin.Identity) plugin.Admission                        { return nil }
func (*bootHost) AdmissionFor(plugin.Identity, plugin.WorkloadKey) plugin.Admission { return nil }
func (*bootHost) RequestShutdown(plugin.Identity, string) bool                      { return false }

func (h *bootHost) SubmitTask(_ plugin.Identity, fn func(context.Context), _ bool) bool {
	if fn == nil {
		return false
	}
	h.tasks.Add(1)
	go func() {
		defer h.tasks.Done()
		fn(h.execution)
	}()
	return true
}

func (h *bootHost) releaseTraffic() { h.gateOnce.Do(func() { close(h.gate) }) }

func (h *bootHost) shutdown() {
	h.cancel()
	h.tasks.Wait()
}

func (h *bootHost) contextFor(key plugin.Key) *plugin.Context {
	return plugin.NewRuntimeContext(h, plugin.Identity{Plugin: key, Instance: plugin.DefaultInstance})
}

// TestHealthProbeAgainstARealServer is the stage's acceptance path: the
// neutral aggregator, this adapter, and the gRPC transport boot through the
// real assembly, and a standard grpc_health_v1 client probes the standard
// service. No credentials are sent, which is itself the check that the
// adapter's public declaration reached the authentication stage.
//
// It also pins the two ends of the NOT_SERVING timeline: before the traffic
// gate releases the direct answer is NOT_SERVING, and once the execution
// context is cancelled -- the moment a real runtime starts draining -- a
// serving server answers NOT_SERVING on the wire while it is still accepting
// RPCs, which is what lets a load balancer stop routing before the listener
// goes away.
func TestHealthProbeAgainstARealServer(t *testing.T) {
	host := newBootHost()
	t.Cleanup(host.shutdown)

	environment, err := config.NewEnvironment(map[string]any{
		"grpc": map[string]any{"addr": "127.0.0.1:0"},
	}, "")
	if err != nil {
		t.Fatalf("NewEnvironment() error = %v", err)
	}
	built, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{grpctransport.Bundle(), Bundle()},
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	// Construct runs every declared Init hook itself, through the lifecycle
	// context this factory builds -- the same runtime context a real host
	// hands to Init. Without the factory the hooks would receive nil.
	constructed, err := assembly.Construct(built, assembly.ConstructOptions{
		ContextFactory: func(identity plugin.Identity, _ log.Logger) *plugin.Context {
			return plugin.NewRuntimeContext(host, identity)
		},
	})
	if err != nil {
		t.Fatalf("Construct() error = %v", err)
	}

	adapter, found := constructed.Instance(plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
	if !found {
		t.Fatal("the health adapter was not constructed")
	}
	healthPlugin, ok := adapter.Primary().(*Plugin)
	if !ok {
		t.Fatalf("health adapter primary = %T, want *Plugin", adapter.Primary())
	}

	// Before the gate releases nothing has been admitted to serve, and the
	// probe says so.
	preGate, err := healthPlugin.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("pre-gate Check() error = %v", err)
	}
	if preGate.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("pre-gate status = %v, want NOT_SERVING", preGate.GetStatus())
	}

	server, found := constructed.Instance(plugin.Identity{Plugin: grpctransport.Key, Instance: plugin.DefaultInstance})
	if !found {
		t.Fatal("the gRPC server was not constructed")
	}
	grpcServer, ok := server.Primary().(*grpctransport.Server)
	if !ok {
		t.Fatalf("gRPC server primary = %T, want *grpc.Server", server.Primary())
	}
	serverContext := host.contextFor(grpctransport.Key)
	if err := grpcServer.Start(serverContext); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	// stopServer runs before the host shutdown registered above (cleanups are
	// LIFO): the serving task ends only when the server it serves is stopped,
	// so an assertion failure after this point must fail fast instead of
	// leaving host.shutdown waiting on a task that never returns.
	t.Cleanup(func() {
		stopContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = grpcServer.Stop(stopContext)
	})
	if err := grpcServer.OpenTraffic(serverContext); err != nil {
		t.Fatalf("OpenTraffic() error = %v", err)
	}
	host.releaseTraffic()

	conn, err := grpc.NewClient(grpcServer.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := healthpb.NewHealthClient(conn)

	waitForStatus(t, client, "", healthpb.HealthCheckResponse_SERVING)

	// Cancelling the execution context is what a runtime does when shutdown
	// begins, before any Stop runs. The server keeps serving, and the probe
	// must already be answering NOT_SERVING through the neutral aggregator's
	// readiness shutdown rule.
	host.cancel()
	waitForStatus(t, client, "", healthpb.HealthCheckResponse_NOT_SERVING)

	if err := conn.Close(); err != nil {
		t.Fatalf("conn.Close() error = %v", err)
	}
	stopContext, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStop()
	if err := grpcServer.Stop(stopContext); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

// waitForStatus polls Check until it reports the wanted status or the deadline
// passes. Polling rather than a fixed sleep keeps the test independent of how
// long the client needs to connect and the server to accept.
func waitForStatus(t *testing.T, client healthpb.HealthClient, service string, want healthpb.HealthCheckResponse_ServingStatus) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last error
	var lastStatus healthpb.HealthCheckResponse_ServingStatus
	for time.Now().Before(deadline) {
		callContext, cancel := context.WithTimeout(context.Background(), time.Second)
		response, err := client.Check(callContext, &healthpb.HealthCheckRequest{Service: service})
		cancel()
		if err == nil {
			last, lastStatus = nil, response.GetStatus()
			if response.GetStatus() == want {
				return
			}
		} else {
			last = err
		}
		time.Sleep(10 * time.Millisecond)
	}
	if last != nil {
		t.Fatalf("health check for %q never succeeded: %v", service, last)
	}
	t.Fatalf("health check for %q = %v, want %v", service, lastStatus, want)
}
