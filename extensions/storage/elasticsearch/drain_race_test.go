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

// TestConcurrentDrainAndStopReportAFlushFailureExactlyOnce forces drainClient
// and stopClient to observe the bulk worker's final flush failure at the same
// time, in both call orders, across many iterations. Across all the errors
// returned by both calls, the failure must appear exactly once: claimBulkErr
// serializes the read and the claim under lifecycleMu, so whichever call
// observes the shared, idempotent bulk.Close result first is the only one
// that gets a non-nil error.
func TestConcurrentDrainAndStopReportAFlushFailureExactlyOnce(t *testing.T) {
	flushErr := errors.New("flush failed")
	for iteration := 0; iteration < 200; iteration++ {
		backend := &stubBackend{performFn: func(*http.Request) (*http.Response, error) { return nil, flushErr }}
		config := validConfig("http://unused.example")
		config.Bulk.FlushActions = 1
		client := constructTestClient(t, config, &fixedFactory{client: newClientWithBackend(backend, time.Second)})
		host := newFakeHost()
		if err := startClient(client, runtimeContext(host, plugin.DefaultInstance)); err != nil {
			t.Fatalf("iteration %d: startClient() error = %v", iteration, err)
		}
		if err := client.Add(context.Background(), BulkItem{Index: "events", Document: []byte(`{}`)}); err != nil {
			t.Fatalf("iteration %d: Add() error = %v", iteration, err)
		}

		var barrier sync.WaitGroup
		barrier.Add(2)
		results := make(chan error, 2)

		runDrain := func() {
			barrier.Done()
			barrier.Wait()
			results <- drainClient(client, context.Background())
		}
		runStop := func() {
			barrier.Done()
			barrier.Wait()
			results <- stopClient(client, context.Background())
		}
		if iteration%2 == 0 {
			go runDrain()
			go runStop()
		} else {
			go runStop()
			go runDrain()
		}

		first := <-results
		second := <-results
		host.waitTasks(t)

		reported := 0
		if first != nil {
			reported++
		}
		if second != nil {
			reported++
		}
		if reported != 1 {
			t.Fatalf("iteration %d: failure reported %d times (drain/stop errors = %v, %v), want exactly once", iteration, reported, first, second)
		}
	}
}

// TestSequentialDrainThenStopReportsOnlyFromDrain pins the ordinary ordering:
// a flush failure observed by drainClient is not repeated by a later
// stopClient.
func TestSequentialDrainThenStopReportsOnlyFromDrain(t *testing.T) {
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
		t.Fatalf("stopClient() error = %v, want nil because drain already reported the failure", err)
	}
	host.waitTasks(t)
}

// TestDrainTimeoutLeavesTheFlushFailureForStop pins the documented timeout
// behavior: when drainClient's context expires before the flush finishes, it
// returns the deadline error and the eventual flush failure belongs to the
// stop that follows, reported exactly once.
func TestDrainTimeoutLeavesTheFlushFailureForStop(t *testing.T) {
	flushErr := errors.New("flush failed")
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	backend := &stubBackend{performFn: func(*http.Request) (*http.Response, error) {
		once.Do(func() { close(started) })
		<-release
		return nil, flushErr
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

	close(release)
	if err := stopClient(client, context.Background()); err == nil {
		t.Fatal("stopClient() error = nil, want the flush failure that drain never observed")
	}
	host.waitTasks(t)

	// A second stop must not repeat the already-reported failure.
	if err := stopClient(client, context.Background()); err == nil {
		t.Fatal("repeated stopClient() error = nil, want the cached failure returned again, not re-derived")
	}
}

// TestStopWithoutDrainReportsTheFlushFailure pins the never-drained path:
// with no drainClient call at all, stopClient is the sole owner of the flush
// failure.
func TestStopWithoutDrainReportsTheFlushFailure(t *testing.T) {
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

	if err := stopClient(client, context.Background()); err == nil {
		t.Fatal("stopClient() error = nil, want the flush failure")
	}
	host.waitTasks(t)
}
