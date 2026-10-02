package webhook

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xbcio/xbc/plugin"
)

// TestDrainClosesAdmissionAndWaitsWithoutCancellingInFlightDelivery pins the
// graceful half of shutdown: drain refuses new deliveries at once, waits for
// the accepted one, and does not abort it when its own budget runs out --
// aborting is left to stop.
func TestDrainClosesAdmissionAndWaitsWithoutCancellingInFlightDelivery(t *testing.T) {
	transport := newBlockingCleanupTransport(false)
	cfg := testConfig()
	cfg.RequestTimeout = 5 * time.Second
	client := newClient(cfg, transport, nil, nil, nil)
	host := newTestRuntimeHost()
	defer host.stopTasks()
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

	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := client.drain(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain() error = %v, want deadline while the delivery is in flight", err)
	}
	if err := client.Enqueue(context.Background(), testDelivery()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Enqueue after drain error = %v, want ErrClosed", err)
	}
	if err := client.runCtx.Err(); err != nil {
		t.Fatalf("an expired drain cancelled in-flight work: %v", err)
	}

	transport.unblock()
	if err := client.drain(context.Background()); err != nil {
		t.Fatalf("drain() after the delivery finished error = %v", err)
	}
	if transport.closeCalls.Load() != 0 {
		t.Fatal("drain closed transport resources; that is stop's job")
	}
	if err := client.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
	if transport.calls.Load() != 1 || transport.closeCalls.Load() != 1 {
		t.Fatalf("transport calls=%d cleanup calls=%d, want 1/1", transport.calls.Load(), transport.closeCalls.Load())
	}
}

// TestDrainBeforeStartReturnsAndLeavesStopSafe covers the abort path, where a
// Drainer may be drained without ever having started its workers.
func TestDrainBeforeStartReturnsAndLeavesStopSafe(t *testing.T) {
	client := newClient(testConfig(), newBlockingCleanupTransport(false), nil, nil, nil)
	if err := client.drain(context.Background()); err != nil {
		t.Fatalf("drain() before Start error = %v", err)
	}
	if err := client.start(testPluginContext(newTestRuntimeHost(), plugin.DefaultInstance)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after drain error = %v, want ErrClosed", err)
	}
	if err := client.stop(context.Background()); err != nil {
		t.Fatalf("stop() after drain error = %v", err)
	}
}
