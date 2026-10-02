package elasticsearch

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/xbcio/xbc/plugin"
)

// TestDrainFlushesAcceptedItemsAndKeepsTheTransportUntilStop pins the
// graceful half of shutdown: drain refuses new items, waits for the accepted
// ones to be flushed, and leaves the transport open for stop to close.
func TestDrainFlushesAcceptedItemsAndKeepsTheTransportUntilStop(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	backend := &stubBackend{performFn: func(request *http.Request) (*http.Response, error) {
		once.Do(func() { close(started) })
		<-release
		return successfulBulkResponse(request)
	}}
	config := validConfig("http://unused.example")
	config.Bulk.FlushActions = 1
	client := constructTestClient(t, config, &fixedFactory{client: newClientWithBackend(backend, time.Second)})
	host := newFakeHost()
	if err := startClient(client, runtimeContext(host, plugin.DefaultInstance)); err != nil {
		t.Fatal(err)
	}
	if err := client.Add(context.Background(), BulkItem{Index: "events", Document: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("bulk request did not start")
	}

	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := drainClient(client, short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drainClient() error = %v, want deadline while the flush is in flight", err)
	}
	if err := client.Add(context.Background(), BulkItem{Index: "events", Document: []byte(`{}`)}); !errors.Is(err, ErrBulkClosed) {
		t.Fatalf("Add after drain error = %v, want ErrBulkClosed", err)
	}
	close(release)
	if err := drainClient(client, context.Background()); err != nil {
		t.Fatalf("drainClient() error = %v", err)
	}
	if backend.closes() != 0 {
		t.Fatal("drain closed the transport; that is Stop's job")
	}
	if stats := client.currentBulk().Stats(); stats.Succeeded != 1 {
		t.Fatalf("succeeded items = %d, want the accepted item flushed", stats.Succeeded)
	}
	if err := stopClient(client, context.Background()); err != nil {
		t.Fatalf("stopClient() error = %v", err)
	}
	host.waitTasks(t)
	if backend.closes() != 1 {
		t.Fatalf("backend closes = %d, want 1", backend.closes())
	}
}

// TestDrainReportsAFlushFailureOnceAndRefusesALaterStart pins that the bulk
// worker's final result is reported by drain and not again by Stop, and that a
// drained client cannot be started.
func TestDrainReportsAFlushFailureOnceAndRefusesALaterStart(t *testing.T) {
	flushErr := errors.New("flush failed")
	backend := &stubBackend{performFn: func(*http.Request) (*http.Response, error) { return nil, flushErr }}
	config := validConfig("http://unused.example")
	config.Bulk.FlushActions = 1
	client := constructTestClient(t, config, &fixedFactory{client: newClientWithBackend(backend, time.Second)})
	host := newFakeHost()
	if err := startClient(client, runtimeContext(host, plugin.DefaultInstance)); err != nil {
		t.Fatal(err)
	}
	if err := client.Add(context.Background(), BulkItem{Index: "events", Document: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := drainClient(client, context.Background()); !errors.Is(err, flushErr) {
		t.Fatalf("drainClient() error = %v, want the flush failure", err)
	}
	if err := stopClient(client, context.Background()); err != nil {
		t.Fatalf("stopClient() error = %v, want the drain's failure not repeated", err)
	}
	host.waitTasks(t)

	unstarted := constructTestClient(t, validConfig("http://unused.example"), &fixedFactory{
		client: newClientWithBackend(&stubBackend{}, time.Second),
	})
	if err := drainClient(unstarted, context.Background()); err != nil {
		t.Fatalf("drainClient() before Start error = %v", err)
	}
	if err := startClient(unstarted, runtimeContext(newFakeHost(), plugin.DefaultInstance)); err == nil {
		t.Fatal("startClient() after drain error = nil")
	}
	if err := stopClient(unstarted, context.Background()); err != nil {
		t.Fatal(err)
	}
}
