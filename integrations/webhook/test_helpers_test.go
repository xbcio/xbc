package webhook

import (
	"context"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func testResponse(request *http.Request, status int, body io.ReadCloser) *http.Response {
	if body == nil {
		body = http.NoBody
	}
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       body,
		Request:    request,
	}
}

func testConfig() Config {
	return Config{
		Workers:          1,
		QueueSize:        1,
		Backpressure:     BackpressureBlock,
		MaxAttempts:      1,
		RequestTimeout:   time.Second,
		MaxPayloadBytes:  1024,
		MaxResponseBytes: 64,
		AllowHTTP:        false,
		MaxRedirects:     3,
		InitialBackoff:   time.Millisecond,
		MaxBackoff:       5 * time.Millisecond,
		Jitter:           0,
		MaxRetryAfter:    30 * time.Millisecond,
	}
}

func testDelivery() Delivery {
	return Delivery{
		ID:      "delivery-1",
		URL:     "https://hooks.example.test/events",
		Event:   "account.updated",
		Payload: []byte(`{"id":1}`),
		Headers: map[string]string{"X-Tenant": "tenant-1"},
		Secret:  []byte("test-secret"),
	}
}

type testRuntimeHost struct {
	mu             sync.Mutex
	managedN       int
	criticalN      int
	accepting      bool
	admissionLimit int
	taskCtx        context.Context
	cancel         context.CancelFunc
	trafficGate    chan struct{}
	wg             sync.WaitGroup
}

func newTestRuntimeHost() *testRuntimeHost {
	ctx, cancel := context.WithCancel(context.Background())
	trafficGate := make(chan struct{})
	close(trafficGate)
	return &testRuntimeHost{
		accepting:      true,
		admissionLimit: -1,
		taskCtx:        ctx,
		cancel:         cancel,
		trafficGate:    trafficGate,
	}
}

func (h *testRuntimeHost) ExecutionContext() context.Context { return h.taskCtx }

func (*testRuntimeHost) Logger() corelog.Logger { return corelog.Nop() }

func (h *testRuntimeHost) TrafficGate() <-chan struct{} { return h.trafficGate }

func (h *testRuntimeHost) SubmitTask(_ plugin.Identity, fn func(context.Context), critical bool) bool {
	h.mu.Lock()
	if !h.accepting || h.admissionLimit >= 0 && h.managedN >= h.admissionLimit {
		h.mu.Unlock()
		return false
	}
	h.managedN++
	if critical {
		h.criticalN++
	}
	h.wg.Add(1)
	ctx := h.taskCtx
	h.mu.Unlock()
	go func() {
		defer h.wg.Done()
		fn(ctx)
	}()
	return true
}

func (*testRuntimeHost) RequestShutdown(plugin.Identity, string) bool { return false }

func (h *testRuntimeHost) setTasksAccepted(value bool) {
	h.mu.Lock()
	h.accepting = value
	h.mu.Unlock()
}

func (h *testRuntimeHost) setAdmissionLimit(limit int) {
	h.mu.Lock()
	h.admissionLimit = limit
	h.mu.Unlock()
}

func (h *testRuntimeHost) managedCounts() (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.managedN, h.criticalN
}

func (h *testRuntimeHost) stopTasks() {
	h.cancel()
	h.wg.Wait()
}

func testPluginContext(host plugin.RuntimeHost, instance string) *plugin.Context {
	return plugin.NewRuntimeContext(host, plugin.Identity{Plugin: Key, Instance: instance})
}

func startManagedTestClient(t *testing.T, client *Client) *testRuntimeHost {
	t.Helper()
	host := newTestRuntimeHost()
	if err := client.start(testPluginContext(host, plugin.DefaultInstance)); err != nil {
		t.Fatalf("start() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = client.stop(ctx)
		host.stopTasks()
	})
	return host
}
