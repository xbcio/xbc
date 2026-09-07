package webhook

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type firstRequestGate struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
}

func newFirstRequestGate() *firstRequestGate {
	return &firstRequestGate{started: make(chan struct{}), release: make(chan struct{})}
}

func (g *firstRequestGate) RoundTrip(request *http.Request) (*http.Response, error) {
	if g.calls.Add(1) == 1 {
		g.once.Do(func() { close(g.started) })
		select {
		case <-g.release:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
	}
	return testResponse(request, http.StatusNoContent, nil), nil
}

func (g *firstRequestGate) unblock() {
	select {
	case <-g.release:
	default:
		close(g.release)
	}
}

func startTestClient(t *testing.T, cfg Config, transport Transport, observer Observer) *Client {
	t.Helper()
	client := newClient(cfg, transport, nil, nil, observer)
	startManagedTestClient(t, client)
	return client
}

func waitStarted(t *testing.T, gate *firstRequestGate) {
	t.Helper()
	select {
	case <-gate.started:
	case <-time.After(time.Second):
		t.Fatal("first queued request did not start")
	}
}

func TestRejectBackpressureUsesHardQueueBound(t *testing.T) {
	cfg := testConfig()
	cfg.Backpressure = BackpressureReject
	cfg.QueueSize = 1
	cfg.RequestTimeout = 5 * time.Second
	gate := newFirstRequestGate()
	t.Cleanup(gate.unblock)
	client := startTestClient(t, cfg, gate, nil)

	if err := client.Enqueue(context.Background(), testDelivery()); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, gate)
	second := testDelivery()
	second.ID = "delivery-2"
	if err := client.Enqueue(context.Background(), second); err != nil {
		t.Fatalf("second Enqueue() error = %v", err)
	}
	third := testDelivery()
	third.ID = "delivery-3"
	if err := client.Enqueue(context.Background(), third); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("third Enqueue() error = %v, want ErrQueueFull", err)
	}
	if got := len(client.queue); got != cfg.QueueSize {
		t.Fatalf("queue length = %d, want hard bound %d", got, cfg.QueueSize)
	}
	gate.unblock()
}

func TestBlockBackpressureWaitsForCapacity(t *testing.T) {
	cfg := testConfig()
	cfg.Backpressure = BackpressureBlock
	cfg.QueueSize = 1
	cfg.RequestTimeout = 5 * time.Second
	gate := newFirstRequestGate()
	t.Cleanup(gate.unblock)
	client := startTestClient(t, cfg, gate, nil)

	if err := client.Enqueue(context.Background(), testDelivery()); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, gate)
	second := testDelivery()
	second.ID = "delivery-2"
	if err := client.Enqueue(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	third := testDelivery()
	third.ID = "delivery-3"
	done := make(chan error, 1)
	go func() { done <- client.Enqueue(context.Background(), third) }()
	select {
	case err := <-done:
		t.Fatalf("blocked Enqueue returned before capacity was available: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	gate.unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("blocked Enqueue() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked Enqueue did not resume when capacity became available")
	}
}

func TestStopDeadlineCancelsInFlightWorkAndWakesBlockedProducer(t *testing.T) {
	cfg := testConfig()
	cfg.Backpressure = BackpressureBlock
	cfg.QueueSize = 1
	cfg.RequestTimeout = 5 * time.Second
	gate := newFirstRequestGate()
	t.Cleanup(gate.unblock)
	client := startTestClient(t, cfg, gate, nil)

	if err := client.Enqueue(context.Background(), testDelivery()); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, gate)
	second := testDelivery()
	second.ID = "delivery-2"
	if err := client.Enqueue(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	third := testDelivery()
	third.ID = "delivery-3"
	producerDone := make(chan error, 1)
	go func() { producerDone <- client.Enqueue(context.Background(), third) }()
	select {
	case err := <-producerDone:
		t.Fatalf("producer returned before shutdown: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := client.stop(ctx); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
	select {
	case err := <-producerDone:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("blocked producer error = %v, want ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not wake blocked producer")
	}
	gate.unblock()
	if err := client.stop(context.Background()); err != nil {
		t.Fatalf("later stop() error = %v", err)
	}
	if err := client.stop(context.Background()); err != nil {
		t.Fatalf("idempotent stop() error = %v", err)
	}
}

func TestEnqueueBeforeStartAndAfterClose(t *testing.T) {
	closedBeforeStart := newClient(testConfig(), roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return testResponse(request, http.StatusNoContent, nil), nil
	}), nil, nil, nil)
	if err := closedBeforeStart.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := closedBeforeStart.Enqueue(context.Background(), testDelivery()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Enqueue after close before start error = %v", err)
	}

	client := newClient(testConfig(), roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return testResponse(request, http.StatusNoContent, nil), nil
	}), nil, nil, nil)
	if err := client.Enqueue(context.Background(), testDelivery()); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Enqueue before start error = %v", err)
	}
	startManagedTestClient(t, client)
	if err := client.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.Enqueue(context.Background(), testDelivery()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Enqueue after close error = %v", err)
	}
}

func TestConcurrentEnqueueAndClose(t *testing.T) {
	cfg := testConfig()
	cfg.QueueSize = 8
	cfg.Workers = 3
	cfg.Backpressure = BackpressureReject
	client := startTestClient(t, cfg, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return testResponse(request, http.StatusNoContent, nil), nil
	}), nil)

	const producers = 32
	var wg sync.WaitGroup
	wg.Add(producers)
	for i := range producers {
		go func(id int) {
			defer wg.Done()
			delivery := testDelivery()
			delivery.ID = "concurrent-" + string(rune('A'+id))
			err := client.Enqueue(context.Background(), delivery)
			if err != nil && !errors.Is(err, ErrQueueFull) && !errors.Is(err, ErrClosed) {
				t.Errorf("Enqueue() error = %v", err)
			}
		}(i)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- client.stop(context.Background()) }()
	wg.Wait()
	if err := <-closeDone; err != nil {
		t.Fatalf("stop() error = %v", err)
	}
}
