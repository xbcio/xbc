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

// TestDrainClosesEnqueueButLeavesDeliverAvailableUntilStop pins the two
// admission paths apart: Drain stops the bounded queue, while a synchronous
// Deliver -- the caller's own bounded call -- keeps working until Stop, which
// refuses it and waits for the calls already in flight.
func TestDrainClosesEnqueueButLeavesDeliverAvailableUntilStop(t *testing.T) {
	transport := newBlockingCleanupTransport(false)
	cfg := testConfig()
	cfg.RequestTimeout = 5 * time.Second
	client := newClient(cfg, transport, nil, nil, nil)
	host := newTestRuntimeHost()
	defer host.stopTasks()
	if err := client.start(testPluginContext(host, plugin.DefaultInstance)); err != nil {
		t.Fatal(err)
	}
	if err := client.drain(context.Background()); err != nil {
		t.Fatalf("drain() error = %v", err)
	}
	if err := client.Enqueue(context.Background(), testDelivery()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Enqueue after drain error = %v, want ErrClosed", err)
	}

	deliveryDone := make(chan error, 1)
	go func() {
		_, err := client.Deliver(context.Background(), testDelivery())
		deliveryDone <- err
	}()
	select {
	case <-transport.started:
	case <-time.After(time.Second):
		t.Fatal("Deliver after drain did not reach the transport")
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- client.stop(context.Background()) }()
	select {
	case err := <-stopDone:
		t.Fatalf("stop() returned before the in-flight delivery finished: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	if _, err := client.Deliver(context.Background(), testDelivery()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Deliver after Stop error = %v, want ErrClosed", err)
	}

	transport.unblock()
	if err := <-deliveryDone; err != nil {
		t.Fatalf("Deliver after drain error = %v", err)
	}
	if err := <-stopDone; err != nil {
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

// TestDrainBeforeTrafficGateReportsDiscardedDeliveries pins that a drain which
// arrives while the workers still wait at the traffic gate does not drop the
// accepted deliveries silently: each is reported to the observer as ErrClosed,
// nothing reaches the transport, and a later drain does not report them again.
func TestDrainBeforeTrafficGateReportsDiscardedDeliveries(t *testing.T) {
	transport := newBlockingCleanupTransport(false)
	cfg := testConfig()
	cfg.QueueSize = 2
	observed := make(chan Result, 2)
	client := newClient(cfg, transport, nil, nil, ObserverFunc(func(_ context.Context, result Result) {
		observed <- result
	}))
	host := newGatedTestRuntimeHost()
	defer host.stopTasks()
	if err := client.start(testPluginContext(host, plugin.DefaultInstance)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"delivery-1", "delivery-2"} {
		delivery := testDelivery()
		delivery.ID = id
		if err := client.Enqueue(context.Background(), delivery); err != nil {
			t.Fatalf("Enqueue(%s) error = %v", id, err)
		}
	}

	if err := client.drain(context.Background()); err != nil {
		t.Fatalf("drain() error = %v", err)
	}
	reported := make(map[string]Result)
	for len(reported) != 2 {
		select {
		case result := <-observed:
			reported[result.DeliveryID] = result
		case <-time.After(time.Second):
			t.Fatalf("drain reported %v, want both discarded deliveries", reported)
		}
	}
	for id, result := range reported {
		if !errors.Is(result.Err, ErrClosed) {
			t.Fatalf("discarded delivery %s error = %v, want ErrClosed", id, result.Err)
		}
	}
	if transport.calls.Load() != 0 {
		t.Fatalf("discarded deliveries reached the transport %d times", transport.calls.Load())
	}

	if err := client.drain(context.Background()); err != nil {
		t.Fatalf("repeated drain() error = %v", err)
	}
	select {
	case result := <-observed:
		t.Fatalf("repeated drain reported %s again", result.DeliveryID)
	default:
	}
	if err := client.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
}
