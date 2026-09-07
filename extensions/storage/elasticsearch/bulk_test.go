package elasticsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testBulkConfig() BulkConfig {
	return BulkConfig{
		QueueCapacity: 32,
		Backpressure:  BackpressureBlock,
		FlushInterval: time.Hour,
		FlushActions:  100,
		FlushBytes:    1 << 20,
	}
}

func bulkOperations(body []byte) ([]string, error) {
	lines := bytes.Split(bytes.TrimSpace(body), []byte("\n"))
	operations := make([]string, 0, len(lines)/2)
	for i := 0; i < len(lines); {
		var metadata map[string]json.RawMessage
		if err := json.Unmarshal(lines[i], &metadata); err != nil {
			return nil, fmt.Errorf("decode metadata line %d: %w", i, err)
		}
		if len(metadata) != 1 {
			return nil, fmt.Errorf("metadata line %d has %d operations", i, len(metadata))
		}
		var operation string
		for operation = range metadata {
		}
		operations = append(operations, operation)
		i++
		if operation != string(BulkDelete) {
			if i >= len(lines) || !json.Valid(lines[i]) {
				return nil, fmt.Errorf("operation %d has no valid document line", len(operations)-1)
			}
			i++
		}
	}
	return operations, nil
}

func successfulBulkResponse(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	operations, err := bulkOperations(body)
	if err != nil {
		return nil, err
	}
	var payload strings.Builder
	payload.WriteString(`{"errors":false,"items":[`)
	for i, operation := range operations {
		if i > 0 {
			payload.WriteByte(',')
		}
		fmt.Fprintf(&payload, `{"%s":{"status":201}}`, operation)
	}
	payload.WriteString(`]}`)
	return response(http.StatusOK, payload.String()), nil
}

func TestBulkIndexerEncodesNDJSONCopiesItemsAndFlushesExplicitly(t *testing.T) {
	var (
		mu     sync.Mutex
		body   []byte
		header http.Header
		method string
		path   string
	)
	backend := &stubBackend{performFn: func(request *http.Request) (*http.Response, error) {
		contents, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		body = append([]byte(nil), contents...)
		header = request.Header.Clone()
		method, path = request.Method, request.URL.Path
		mu.Unlock()
		request.Body = io.NopCloser(bytes.NewReader(contents))
		return successfulBulkResponse(request)
	}}
	indexer := newAsyncBulkIndexer(newClientWithBackend(backend, time.Second), testBulkConfig(), nil)
	document := []byte(`{ "name": "first" }`)
	if err := indexer.Add(context.Background(), BulkItem{Operation: BulkIndex, Index: " events ", ID: "1", Routing: "tenant-a", Document: document}); err != nil {
		t.Fatal(err)
	}
	document[2] = 'X'
	if err := indexer.Add(context.Background(), BulkItem{Operation: BulkDelete, Index: "events", ID: "2"}); err != nil {
		t.Fatal(err)
	}
	if err := indexer.Flush(context.Background()); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if err := indexer.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	mu.Lock()
	gotBody := string(body)
	gotHeader, gotMethod, gotPath := header, method, path
	mu.Unlock()
	wantBody := "{\"index\":{\"_id\":\"1\",\"_index\":\"events\",\"routing\":\"tenant-a\"}}\n" +
		"{\"name\":\"first\"}\n" +
		"{\"delete\":{\"_id\":\"2\",\"_index\":\"events\"}}\n"
	if gotBody != wantBody {
		t.Fatalf("bulk body:\n%s\nwant:\n%s", gotBody, wantBody)
	}
	if gotMethod != http.MethodPost || gotPath != "/_bulk" {
		t.Fatalf("bulk request = %s %s", gotMethod, gotPath)
	}
	if got := gotHeader.Get("Content-Type"); got != "application/x-ndjson" {
		t.Fatalf("Content-Type = %q", got)
	}
	if stats := indexer.Stats(); stats != (BulkStats{Added: 2, Flushed: 2, Succeeded: 2, Requests: 1}) {
		t.Fatalf("Stats() = %+v", stats)
	}
}

func TestBulkIndexerAutomaticFlushTriggers(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*BulkConfig)
		flush     bool
	}{
		{name: "actions", configure: func(cfg *BulkConfig) { cfg.FlushActions = 2 }},
		{name: "bytes", configure: func(cfg *BulkConfig) { cfg.FlushBytes = 1 }},
		{name: "interval", configure: func(cfg *BulkConfig) { cfg.FlushInterval = 5 * time.Millisecond }},
		{name: "explicit", flush: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			started := make(chan struct{})
			var once sync.Once
			backend := &stubBackend{performFn: func(request *http.Request) (*http.Response, error) {
				requests.Add(1)
				once.Do(func() { close(started) })
				return successfulBulkResponse(request)
			}}
			cfg := testBulkConfig()
			if test.configure != nil {
				test.configure(&cfg)
			}
			indexer := newAsyncBulkIndexer(newClientWithBackend(backend, time.Second), cfg, nil)
			if err := indexer.Add(context.Background(), BulkItem{Index: "events", ID: "1", Document: []byte(`{"ok":true}`)}); err != nil {
				t.Fatal(err)
			}
			if test.name == "actions" {
				if err := indexer.Add(context.Background(), BulkItem{Index: "events", ID: "2", Document: []byte(`{"ok":true}`)}); err != nil {
					t.Fatal(err)
				}
			}
			if test.flush {
				if err := indexer.Flush(context.Background()); err != nil {
					t.Fatalf("Flush() error = %v", err)
				}
			}
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatalf("%s trigger did not flush", test.name)
			}
			if err := indexer.Close(context.Background()); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("requests = %d, want 1", got)
			}
		})
	}
}

func TestBulkIndexerReportsItemFailuresToObserversAndStats(t *testing.T) {
	backend := &stubBackend{performFn: func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"errors":true,"items":[{"index":{"status":201}},{"create":{"status":409,"error":{"type":"version_conflict_engine_exception","reason":"already exists"}}}]}`), nil
	}}
	var (
		mu          sync.Mutex
		global      []BulkResult
		itemResults []BulkResult
	)
	globalObserver := BulkObserverFunc(func(result BulkResult) {
		mu.Lock()
		global = append(global, result)
		mu.Unlock()
	})
	itemObserver := BulkObserverFunc(func(result BulkResult) {
		mu.Lock()
		itemResults = append(itemResults, result)
		mu.Unlock()
	})
	indexer := newAsyncBulkIndexer(newClientWithBackend(backend, time.Second), testBulkConfig(), globalObserver)
	if err := indexer.Add(context.Background(), BulkItem{Index: "events", ID: "1", Document: []byte(`{"ok":true}`)}); err != nil {
		t.Fatal(err)
	}
	if err := indexer.Add(context.Background(), BulkItem{Operation: BulkCreate, Index: "events", ID: "2", Document: []byte(`{"secret":"not-in-error"}`), Observer: itemObserver}); err != nil {
		t.Fatal(err)
	}
	err := indexer.Flush(context.Background())
	var itemErr *BulkItemError
	if !errors.As(err, &itemErr) {
		t.Fatalf("Flush() error = %v, want *BulkItemError", err)
	}
	if itemErr.Status != http.StatusConflict || itemErr.Type != "version_conflict_engine_exception" || itemErr.ID != "2" {
		t.Fatalf("BulkItemError = %+v", itemErr)
	}
	if strings.Contains(err.Error(), "not-in-error") {
		t.Fatalf("BulkItemError exposed source document: %v", err)
	}
	if closeErr := indexer.Close(context.Background()); !errors.As(closeErr, &itemErr) {
		t.Fatalf("Close() error = %v, want remembered item failure", closeErr)
	}
	mu.Lock()
	gotGlobal := append([]BulkResult(nil), global...)
	gotItem := append([]BulkResult(nil), itemResults...)
	mu.Unlock()
	if len(gotGlobal) != 2 || len(gotItem) != 1 || gotItem[0].Error == nil {
		t.Fatalf("observer results: global=%d item=%+v", len(gotGlobal), gotItem)
	}
	wantStats := BulkStats{Added: 2, Flushed: 2, Succeeded: 1, Failed: 1, Requests: 1}
	if stats := indexer.Stats(); stats != wantStats {
		t.Fatalf("Stats() = %+v, want %+v", stats, wantStats)
	}
}

func TestBulkIndexerBackpressurePolicies(t *testing.T) {
	for _, policy := range []string{BackpressureReject, BackpressureBlock} {
		t.Run(policy, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			backend := &stubBackend{performFn: func(request *http.Request) (*http.Response, error) {
				once.Do(func() { close(started) })
				<-release
				return successfulBulkResponse(request)
			}}
			cfg := testBulkConfig()
			cfg.QueueCapacity = 1
			cfg.Backpressure = policy
			cfg.FlushActions = 1
			indexer := newAsyncBulkIndexer(newClientWithBackend(backend, time.Second), cfg, nil)
			item := BulkItem{Index: "events", Document: []byte(`{"ok":true}`)}
			if err := indexer.Add(context.Background(), item); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("first request did not block in backend")
			}
			if err := indexer.Add(context.Background(), item); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
			err := indexer.Add(ctx, item)
			cancel()
			if policy == BackpressureReject {
				if !errors.Is(err, ErrBulkQueueFull) {
					t.Fatalf("Add() error = %v, want queue full", err)
				}
			} else if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Add() error = %v, want deadline exceeded", err)
			}
			close(release)
			if err := indexer.Close(context.Background()); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			stats := indexer.Stats()
			if stats.Added != 2 || stats.Flushed != 2 {
				t.Fatalf("Stats() = %+v", stats)
			}
			if policy == BackpressureReject && stats.Rejected != 1 {
				t.Fatalf("Rejected = %d, want 1", stats.Rejected)
			}
		})
	}
}

func TestBulkIndexerConcurrentProducersAndCloseFlushEveryAcceptedItem(t *testing.T) {
	backend := &stubBackend{performFn: successfulBulkResponse}
	cfg := testBulkConfig()
	cfg.QueueCapacity = 128
	indexer := newAsyncBulkIndexer(newClientWithBackend(backend, time.Second), cfg, nil)

	const producers = 64
	var producersWG sync.WaitGroup
	for i := 0; i < producers; i++ {
		producersWG.Add(1)
		go func(id int) {
			defer producersWG.Done()
			err := indexer.Add(context.Background(), BulkItem{Index: "events", ID: fmt.Sprint(id), Document: []byte(`{"ok":true}`)})
			if err != nil {
				t.Errorf("Add(%d) error = %v", id, err)
			}
		}(i)
	}
	producersWG.Wait()

	const closers = 8
	results := make(chan error, closers)
	for range closers {
		go func() { results <- indexer.Close(context.Background()) }()
	}
	for range closers {
		if err := <-results; err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}
	if err := indexer.Add(context.Background(), BulkItem{Index: "events", Document: []byte(`{}`)}); !errors.Is(err, ErrBulkClosed) {
		t.Fatalf("Add() after Close error = %v, want ErrBulkClosed", err)
	}
	stats := indexer.Stats()
	if stats.Added != producers || stats.Flushed != producers || stats.Succeeded != producers {
		t.Fatalf("Stats() = %+v, want all %d accepted items flushed", stats, producers)
	}
}

func TestBulkIndexerAutomaticFailureReachesNextFlushBarrier(t *testing.T) {
	sentinel := errors.New("automatic flush failed")
	requestDone := make(chan struct{})
	backend := &stubBackend{performFn: func(*http.Request) (*http.Response, error) {
		close(requestDone)
		return nil, sentinel
	}}
	cfg := testBulkConfig()
	cfg.FlushActions = 1
	indexer := newAsyncBulkIndexer(newClientWithBackend(backend, time.Second), cfg, nil)
	if err := indexer.Add(context.Background(), BulkItem{Index: "events", Document: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("automatic flush did not run")
	}
	if err := indexer.Flush(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("Flush() error = %v, want automatic failure", err)
	}
	if err := indexer.Flush(context.Background()); err != nil {
		t.Fatalf("second barrier repeated an acknowledged failure: %v", err)
	}
	if err := indexer.Close(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("Close() error = %v, want lifetime failure", err)
	}
}

func TestBulkIndexerContainsObserverPanicAndKeepsWorkerAlive(t *testing.T) {
	backend := &stubBackend{performFn: successfulBulkResponse}
	indexer := newAsyncBulkIndexer(newClientWithBackend(backend, time.Second), testBulkConfig(), nil)
	if err := indexer.Add(context.Background(), BulkItem{
		Index: "events", Document: []byte(`{"first":true}`),
		Observer: BulkObserverFunc(func(BulkResult) { panic("secret observer panic") }),
	}); err != nil {
		t.Fatal(err)
	}
	err := indexer.Flush(context.Background())
	if err == nil || !strings.Contains(err.Error(), "item bulk observer panicked") {
		t.Fatalf("Flush() error = %v, want contained observer panic", err)
	}
	if strings.Contains(err.Error(), "secret observer panic") {
		t.Fatalf("observer panic value leaked: %v", err)
	}
	if err := indexer.Add(context.Background(), BulkItem{Index: "events", Document: []byte(`{"second":true}`)}); err != nil {
		t.Fatalf("worker stopped after observer panic: %v", err)
	}
	if err := indexer.Flush(context.Background()); err != nil {
		t.Fatalf("worker failed later flush after observer panic: %v", err)
	}
	if err := indexer.Close(context.Background()); err == nil || !strings.Contains(err.Error(), "observer panicked") {
		t.Fatalf("Close() error = %v, want remembered observer failure", err)
	}
}

func TestBulkIndexerCloseDeadlineIsNotBlockedByFullQueueAdmission(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	var once sync.Once
	backend := &stubBackend{performFn: func(request *http.Request) (*http.Response, error) {
		once.Do(func() { close(requestStarted) })
		<-releaseRequest
		return successfulBulkResponse(request)
	}}
	cfg := testBulkConfig()
	cfg.QueueCapacity = 1
	cfg.FlushActions = 1
	indexer := newAsyncBulkIndexer(newClientWithBackend(backend, time.Second), cfg, nil)
	item := BulkItem{Index: "events", Document: []byte(`{}`)}
	if err := indexer.Add(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not block worker")
	}
	if err := indexer.Add(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	blockedAdd := make(chan error, 1)
	go func() { blockedAdd <- indexer.Add(context.Background(), item) }()
	select {
	case err := <-blockedAdd:
		t.Fatalf("third Add was not blocked: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	if err := indexer.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close() error = %v, want deadline exceeded", err)
	}
	select {
	case err := <-blockedAdd:
		if !errors.Is(err, ErrBulkClosed) {
			t.Fatalf("blocked Add error = %v, want ErrBulkClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked Add did not observe close")
	}
	close(releaseRequest)
	if err := indexer.Close(context.Background()); err != nil {
		t.Fatalf("fresh Close() error = %v", err)
	}
}

func TestNormalizeBulkItemRejectsInvalidOperationsAndDocuments(t *testing.T) {
	tests := []BulkItem{
		{Document: []byte(`{}`)},
		{Index: "events"},
		{Index: "events", Document: []byte(`not-json`)},
		{Operation: BulkDelete, Index: "events", Document: []byte(`{}`)},
		{Operation: "upsert", Index: "events", Document: []byte(`{}`)},
	}
	for _, item := range tests {
		if _, err := normalizeBulkItem(item); err == nil {
			t.Fatalf("normalizeBulkItem(%+v) unexpectedly succeeded", item)
		}
	}
	if _, err := normalizeBulkItem(BulkItem{Operation: BulkDelete, Index: "events"}); err != nil {
		t.Fatalf("normalizeBulkItem(delete) error = %v", err)
	}
	if _, err := normalizeBulkItem(BulkItem{Index: "events", Document: []byte(`{}`)}); err != nil {
		t.Fatalf("normalizeBulkItem(default index) error = %v", err)
	}
	if !reflect.DeepEqual([]BulkOperation{BulkIndex, BulkCreate, BulkUpdate, BulkDelete}, []BulkOperation{"index", "create", "update", "delete"}) {
		t.Fatal("bulk operation constants changed unexpectedly")
	}
}
