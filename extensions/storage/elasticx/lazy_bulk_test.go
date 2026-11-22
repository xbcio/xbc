package elasticx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// newBulkTestServer answers /_bulk with one successful item per submitted
// operation and reports each request's operations to the returned channel.
func newBulkTestServer(t *testing.T) (*httptest.Server, <-chan []string) {
	t.Helper()
	requests := make(chan []string, 16)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/_bulk" {
			http.NotFound(writer, request)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		operations, err := bulkOperations(body)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		requests <- operations
		var payload strings.Builder
		payload.WriteString(`{"errors":false,"items":[`)
		for i, operation := range operations {
			if i > 0 {
				payload.WriteByte(',')
			}
			fmt.Fprintf(&payload, `{"%s":{"status":201}}`, operation)
		}
		payload.WriteString(`]}`)
		writer.Header().Set("X-Elastic-Product", "Elasticsearch")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, payload.String())
	}))
	t.Cleanup(server.Close)
	return server, requests
}

// TestNewStartsBulkWorkerLazilyOnFirstAdd pins the lazy-start behavior: a New
// client owns no bulk worker until the first Add, and Close stops that worker
// and refuses later additions.
func TestNewStartsBulkWorkerLazilyOnFirstAdd(t *testing.T) {
	server, requests := newBulkTestServer(t)
	config := validConfig(server.URL)
	config.Bulk.FlushActions = 1
	client, err := New(config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	if client.currentBulk() != nil {
		t.Fatal("New started the bulk worker eagerly")
	}
	if stats := client.Stats(); stats != (BulkStats{}) {
		t.Fatalf("Stats() before first use = %+v, want zero", stats)
	}

	if err := client.Add(context.Background(), BulkItem{Index: "events", Document: []byte(`{"n":1}`)}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if client.currentBulk() == nil {
		t.Fatal("the first Add did not start the bulk worker")
	}
	select {
	case operations := <-requests:
		if len(operations) != 1 {
			t.Fatalf("bulk request carried %d operations, want 1", len(operations))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bulk request did not arrive")
	}
	if err := client.Flush(context.Background()); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if stats := client.Stats(); stats != (BulkStats{Added: 1, Flushed: 1, Succeeded: 1, Requests: 1}) {
		t.Fatalf("Stats() = %+v", stats)
	}

	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if client.currentBulk() != nil {
		t.Fatal("Close left the bulk worker installed")
	}
	if err := client.Add(context.Background(), BulkItem{Index: "events", Document: []byte(`{"n":2}`)}); !errors.Is(err, ErrBulkClosed) {
		t.Fatalf("Add() after Close error = %v, want ErrBulkClosed", err)
	}
}

// TestNewCloseBeforeBulkUseDoesNotStartOrBlock pins the unused-client half:
// Close must neither create a worker nor wait on one.
func TestNewCloseBeforeBulkUseDoesNotStartOrBlock(t *testing.T) {
	client, err := New(validConfig("http://127.0.0.1:1"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if client.currentBulk() != nil {
		t.Fatal("New started the bulk worker eagerly")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	startedAt := time.Now()
	if err := client.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("Close() without a bulk worker took %s", elapsed)
	}
}

// TestNewFirstFlushStartsBulkWorker pins the other lazy entry point: Flush is a
// bulk operation, so it creates the worker rather than reporting a closed one.
func TestNewFirstFlushStartsBulkWorker(t *testing.T) {
	server, _ := newBulkTestServer(t)
	client, err := New(validConfig(server.URL))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	if client.currentBulk() != nil {
		t.Fatal("New started the bulk worker eagerly")
	}
	if err := client.Flush(context.Background()); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if client.currentBulk() == nil {
		t.Fatal("the first Flush did not start the bulk worker")
	}
}

// TestNewBulkWorkerStartsOnceUnderConcurrentAdds drives concurrent producers
// through a client with no worker yet, then verifies one worker carried the
// entire batch in a single request.
func TestNewBulkWorkerStartsOnceUnderConcurrentAdds(t *testing.T) {
	server, requests := newBulkTestServer(t)
	config := validConfig(server.URL)
	config.Bulk.FlushInterval = time.Hour
	config.Bulk.FlushActions = 1000
	config.Bulk.FlushBytes = 1 << 20
	client, err := New(config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	const producers = 32
	results := make(chan error, producers)
	var wait sync.WaitGroup
	for i := 0; i < producers; i++ {
		wait.Add(1)
		go func(n int) {
			defer wait.Done()
			results <- client.Add(context.Background(), BulkItem{Index: "events", Document: []byte(fmt.Sprintf(`{"n":%d}`, n))})
		}(i)
	}
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent Add() error = %v", err)
		}
	}
	if err := client.Flush(context.Background()); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	select {
	case operations := <-requests:
		if len(operations) != producers {
			t.Fatalf("one flush carried %d operations, want %d", len(operations), producers)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bulk request did not arrive")
	}
	if stats := client.Stats(); stats != (BulkStats{Added: producers, Flushed: producers, Succeeded: producers, Requests: 1}) {
		t.Fatalf("Stats() = %+v", stats)
	}
}
