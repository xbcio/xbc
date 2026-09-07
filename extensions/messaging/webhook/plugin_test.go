package webhook

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xbcio/xbc/plugin"
)

func TestClientStartSubmitsOnlyConfiguredCriticalWorkers(t *testing.T) {
	cfg := testConfig()
	cfg.Workers = 3
	client := newClient(cfg, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return testResponse(request, http.StatusNoContent, nil), nil
	}), nil, nil, nil)
	host := newTestRuntimeHost()
	ctx := testPluginContext(host, "secondary")

	if err := client.Enqueue(context.Background(), testDelivery()); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Enqueue before Start error = %v", err)
	}
	if managed, critical := host.managedCounts(); managed != 0 || critical != 0 {
		t.Fatalf("construction started workers: managed=%d critical=%d", managed, critical)
	}
	if err := client.start(ctx); err != nil {
		t.Fatalf("start() error = %v", err)
	}
	managed, critical := host.managedCounts()
	if managed != cfg.Workers || critical != cfg.Workers {
		t.Fatalf("managed workers = %d critical = %d, want %d", managed, critical, cfg.Workers)
	}
	if err := client.start(ctx); err == nil {
		t.Fatal("second Start error = nil")
	}
	if err := client.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
	if err := client.stop(context.Background()); err != nil {
		t.Fatalf("second stop() error = %v", err)
	}
	host.stopTasks()
}

func TestClientStartHandlesManagedTaskAdmissionRejection(t *testing.T) {
	t.Run("first worker", func(t *testing.T) {
		cfg := testConfig()
		cfg.Workers = 2
		client := newClient(cfg, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return testResponse(request, http.StatusNoContent, nil), nil
		}), nil, nil, nil)
		host := newTestRuntimeHost()
		host.setTasksAccepted(false)

		err := client.start(testPluginContext(host, plugin.DefaultInstance))
		if err == nil || !errors.Is(client.Enqueue(context.Background(), testDelivery()), ErrClosed) {
			t.Fatalf("rejected Start error = %v, Enqueue did not close", err)
		}
		if managed, critical := host.managedCounts(); managed != 0 || critical != 0 {
			t.Fatalf("rejected Start admitted managed=%d critical=%d", managed, critical)
		}
		if err := client.stop(context.Background()); err != nil {
			t.Fatalf("stop after rejected Start error = %v", err)
		}
		if err := client.stop(context.Background()); err != nil {
			t.Fatalf("idempotent stop after rejected Start error = %v", err)
		}
		host.stopTasks()
	})

	t.Run("partial workers", func(t *testing.T) {
		cfg := testConfig()
		cfg.Workers = 3
		client := newClient(cfg, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return testResponse(request, http.StatusNoContent, nil), nil
		}), nil, nil, nil)
		host := newTestRuntimeHost()
		host.setAdmissionLimit(1)

		err := client.start(testPluginContext(host, plugin.DefaultInstance))
		if err == nil {
			t.Fatal("partially admitted Start error = nil")
		}
		if managed, critical := host.managedCounts(); managed != 1 || critical != 1 {
			t.Fatalf("partially admitted workers: managed=%d critical=%d", managed, critical)
		}
		if err := client.stop(context.Background()); err != nil {
			t.Fatalf("stop after partial Start error = %v", err)
		}
		if err := client.stop(context.Background()); err != nil {
			t.Fatalf("idempotent stop after partial Start error = %v", err)
		}
		host.stopTasks()
	})
}

type blockingCleanupTransport struct {
	started     chan struct{}
	release     chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once
	calls       atomic.Int32
	closeCalls  atomic.Int32
	panicClose  bool
}

func newBlockingCleanupTransport(panicClose bool) *blockingCleanupTransport {
	return &blockingCleanupTransport{
		started:    make(chan struct{}),
		release:    make(chan struct{}),
		panicClose: panicClose,
	}
}

func (t *blockingCleanupTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	t.startOnce.Do(func() { close(t.started) })
	select {
	case <-t.release:
		return testResponse(request, http.StatusNoContent, nil), nil
	case <-request.Context().Done():
		return nil, request.Context().Err()
	}
}

func (t *blockingCleanupTransport) CloseIdleConnections() {
	t.closeCalls.Add(1)
	if t.panicClose {
		panic("cleanup failed")
	}
}

func (t *blockingCleanupTransport) unblock() {
	t.releaseOnce.Do(func() { close(t.release) })
}

func TestClientStopDeadlineCancelsDrainAndSharesIdempotentResult(t *testing.T) {
	transport := newBlockingCleanupTransport(true)
	cfg := testConfig()
	cfg.RequestTimeout = 5 * time.Second
	client := newClient(cfg, transport, nil, nil, nil)
	host := newTestRuntimeHost()
	if err := client.start(testPluginContext(host, plugin.DefaultInstance)); err != nil {
		t.Fatal(err)
	}
	if err := client.Enqueue(context.Background(), testDelivery()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-transport.started:
	case <-time.After(time.Second):
		t.Fatal("queued delivery did not start")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	results := make(chan error, 2)
	go func() { results <- client.stop(stopCtx) }()
	go func() { results <- client.stop(context.Background()) }()
	first, second := <-results, <-results
	if first == nil || first.Error() != "webhook: transport cleanup panicked" {
		t.Fatalf("first shared stop error = %v", first)
	}
	if second != first {
		t.Fatalf("concurrent Stop returned different errors: %p and %p", first, second)
	}
	if later := client.stop(context.Background()); later != first {
		t.Fatalf("later Stop error = %v, want shared %v", later, first)
	}
	if err := client.Enqueue(context.Background(), testDelivery()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Enqueue after Stop error = %v", err)
	}
	if transport.calls.Load() != 1 || transport.closeCalls.Load() != 1 {
		t.Fatalf("transport calls=%d cleanup calls=%d", transport.calls.Load(), transport.closeCalls.Load())
	}
	transport.unblock()
	host.stopTasks()
}

func TestClientLifecycleValidationAndStopBeforeStart(t *testing.T) {
	if err := (*Client)(nil).start(nil); err == nil {
		t.Fatal("Start(nil) error = nil")
	}

	client := newClient(testConfig(), roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return testResponse(request, http.StatusNoContent, nil), nil
	}), nil, nil, nil)
	if err := client.stop(context.Background()); err != nil {
		t.Fatalf("Stop before Start error = %v", err)
	}
	if err := client.stop(context.Background()); err != nil {
		t.Fatalf("second Stop before Start error = %v", err)
	}
	if err := client.start(testPluginContext(newTestRuntimeHost(), plugin.DefaultInstance)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Stop error = %v, want ErrClosed", err)
	}
}
