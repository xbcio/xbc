package gracefulshutdown

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

type testHost struct {
	mu sync.Mutex

	calls     int
	identity  plugin.Identity
	reason    string
	stopOnce  sync.Once
	execution context.Context
	cancel    context.CancelFunc
}

var _ plugin.RuntimeHost = (*testHost)(nil)

func newTestHost() *testHost {
	execution, cancel := context.WithCancel(context.Background())
	return &testHost{execution: execution, cancel: cancel}
}

func (h *testHost) ExecutionContext() context.Context        { return h.execution }
func (*testHost) Logger() log.Logger                         { return log.Nop() }
func (*testHost) ProcessInstance() string                    { return "test-process" }
func (*testHost) TrafficGate() <-chan struct{}               { return nil }
func (*testHost) Admission(plugin.Identity) plugin.Admission { return nil }

func (*testHost) AdmissionFor(plugin.Identity, plugin.WorkloadKey) plugin.Admission { return nil }

func (*testHost) SubmitTask(plugin.Identity, func(context.Context), bool) bool {
	return false
}

func (h *testHost) RequestShutdown(identity plugin.Identity, reason string) bool {
	accepted := false
	h.stopOnce.Do(func() {
		accepted = true
		h.mu.Lock()
		h.calls++
		h.identity = identity
		h.reason = reason
		h.mu.Unlock()
		h.cancel()
	})
	return accepted
}

func initializedController(t *testing.T) (*Controller, *testHost) {
	t.Helper()
	controller := New()
	host := newTestHost()
	ctx := plugin.NewRuntimeContext(host, plugin.Identity{Plugin: Key})
	if err := controller.Init(ctx); err != nil {
		t.Fatal(err)
	}
	return controller, host
}

// TestRequestIsAcceptedOnceAndNormalizesItsReason pins Request's two load
// bearing behaviors together: only the first caller is told its request was
// accepted, and the reason that reaches the runtime is whitespace-normalized,
// with a blank reason replaced by a stable default.
func TestRequestIsAcceptedOnceAndNormalizesItsReason(t *testing.T) {
	controller, host := initializedController(t)
	if !controller.Request(" operator\nrequest ") {
		t.Fatal("first shutdown request was not accepted")
	}
	if controller.Request("again") {
		t.Fatal("second shutdown request was accepted")
	}
	if !controller.ShuttingDown() {
		t.Fatal("controller did not observe lifecycle cancellation")
	}

	host.mu.Lock()
	defer host.mu.Unlock()
	if host.calls != 1 || host.identity.Plugin != Key || host.reason != "operator request" {
		t.Fatalf("host shutdown = calls %d, identity %v, reason %q", host.calls, host.identity, host.reason)
	}
}

// TestRequestWithBlankReasonUsesOperatorRequestDefault covers reason
// normalization in isolation from acceptance, so a change to the default
// string is caught here rather than only as a side effect of the test above.
func TestRequestWithBlankReasonUsesOperatorRequestDefault(t *testing.T) {
	controller, host := initializedController(t)
	if !controller.Request("   ") {
		t.Fatal("request with blank reason was not accepted")
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.reason != "operator request" {
		t.Fatalf("host reason = %q, want %q", host.reason, "operator request")
	}
}

// TestRequestOnUnboundControllerReturnsFalse covers both ways a Controller can
// be unbound: never initialized, and initialized then Stopped.
func TestRequestOnUnboundControllerReturnsFalse(t *testing.T) {
	if New().Request("never initialized") {
		t.Fatal("a Controller that was never Init'd accepted a shutdown request")
	}

	controller, _ := initializedController(t)
	if err := controller.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if controller.Request("after stop") {
		t.Fatal("a Controller detached by Stop accepted a shutdown request")
	}
}

// TestNilControllerIsSafe covers the typed-nil-receiver guard every exported
// method carries, independent from the unbound-but-non-nil case above.
func TestNilControllerIsSafe(t *testing.T) {
	var controller *Controller
	if controller.Request("reason") {
		t.Fatal("nil Controller accepted a shutdown request")
	}
	if controller.ShuttingDown() {
		t.Fatal("nil Controller reported ShuttingDown")
	}
}

func TestControllerInitRequiresContext(t *testing.T) {
	if err := New().Init(nil); err == nil {
		t.Fatal("Init(nil) succeeded")
	}
}

func TestShuttingDownReflectsLifecycleCancellation(t *testing.T) {
	controller, host := initializedController(t)
	if controller.ShuttingDown() {
		t.Fatal("controller reported ShuttingDown before any request")
	}
	host.cancel()
	if !controller.ShuttingDown() {
		t.Fatal("controller did not observe direct lifecycle cancellation")
	}
}

func TestStopDetachesController(t *testing.T) {
	controller, _ := initializedController(t)
	if err := controller.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if controller.Request("after stop") {
		t.Fatal("detached controller accepted shutdown")
	}
}

// TestConcurrentRequestsAcceptExactlyOne drives many concurrent callers at one
// Controller and checks that acceptance is exclusive even under race detection:
// Init's bind and every Request and ShuttingDown call share the same mutex.
func TestConcurrentRequestsAcceptExactlyOne(t *testing.T) {
	controller, host := initializedController(t)

	const callers = 64
	var wg sync.WaitGroup
	var accepted atomic.Int32
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			if controller.Request("concurrent request") {
				accepted.Add(1)
			}
			controller.ShuttingDown()
		}()
	}
	wg.Wait()

	if accepted.Load() != 1 {
		t.Fatalf("accepted = %d, want exactly 1 caller told its request was accepted", accepted.Load())
	}

	host.mu.Lock()
	defer host.mu.Unlock()
	if host.calls != 1 {
		t.Fatalf("host.calls = %d, want exactly 1 accepted shutdown", host.calls)
	}
}

func TestDefinitionIsCanonicalAndBundleContainsIt(t *testing.T) {
	first := Definition()
	second := Definition()
	if first != second {
		t.Fatal("Definition() returned different declaration handles")
	}

	entries := pluginmodel.BundleEntries(pluginmodel.Bundle(Bundle()))
	if len(entries) != 1 {
		t.Fatalf("Bundle() entries = %d, want 1", len(entries))
	}
	if !pluginmodel.SameDefinition(entries[0].Definition, pluginmodel.Definition(first)) {
		t.Fatal("Bundle() does not contain the canonical Definition handle")
	}

	descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(first))
	if !ok {
		t.Fatal("Definition() returned a zero handle")
	}
	if descriptor.Key != pluginmodel.Key(Key) {
		t.Fatalf("Definition key = %v, want %v", descriptor.Key, Key)
	}
	if descriptor.Activation.Kind != pluginmodel.ActivationConfigured || descriptor.Activation.Path != "plugins.gracefulshutdown" {
		t.Fatalf("Definition activation = %+v, want ActivationConfigured at plugins.gracefulshutdown", descriptor.Activation)
	}
	if descriptor.Primary != reflect.TypeOf((*Controller)(nil)) {
		t.Fatalf("Definition primary = %v, want *Controller", descriptor.Primary)
	}
	if descriptor.Config != nil {
		t.Fatalf("Definition config = %+v, want no ConfigSpec", descriptor.Config)
	}
	if len(descriptor.Contracts) != 0 {
		t.Fatalf("Definition contracts = %+v, want no exports beyond the primary", descriptor.Contracts)
	}
}
