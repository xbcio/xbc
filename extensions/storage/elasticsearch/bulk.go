package elasticsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrBulkClosed    = errors.New("elasticsearch: bulk indexer is closed")
	ErrBulkQueueFull = errors.New("elasticsearch: bulk queue is full")
)

// BulkOperation is an Elasticsearch bulk action name.
type BulkOperation string

const (
	BulkIndex  BulkOperation = "index"
	BulkCreate BulkOperation = "create"
	BulkUpdate BulkOperation = "update"
	BulkDelete BulkOperation = "delete"
)

// BulkItem is copied by Add. Document must contain one valid JSON value for
// index/create/update operations and must be empty for delete.
type BulkItem struct {
	Operation BulkOperation
	Index     string
	ID        string
	Routing   string
	Document  []byte
	Observer  BulkObserver
}

// BulkResult reports one accepted item's outcome. Error is non-nil for
// transport, malformed-response, and Elasticsearch item-level failures.
type BulkResult struct {
	Item        BulkItem
	Status      int
	Error       error
	ErrorType   string
	ErrorReason string
}

// BulkObserver receives one callback per accepted item. Callbacks execute on
// the single bulk worker. Panics are contained and returned by Flush/Close;
// callbacks must still return quickly to avoid delaying the worker.
type BulkObserver interface{ Observe(BulkResult) }

// BulkObserverFunc adapts a function to BulkObserver.
type BulkObserverFunc func(BulkResult)

func (f BulkObserverFunc) Observe(result BulkResult) { f(result) }

// BulkStats is a lock-free snapshot. Flushed counts items with a known result,
// including failures; Rejected counts only reject-policy queue rejections.
type BulkStats struct {
	Added     uint64
	Flushed   uint64
	Succeeded uint64
	Failed    uint64
	Rejected  uint64
	Requests  uint64
}

// BulkIndexer is the application-facing bounded asynchronous indexing API.
type BulkIndexer interface {
	Add(context.Context, BulkItem) error
	Flush(context.Context) error
	Stats() BulkStats
	Close(context.Context) error
}

type flushRequest struct{ done chan error }

type bulkCounters struct {
	added     atomic.Uint64
	flushed   atomic.Uint64
	succeeded atomic.Uint64
	failed    atomic.Uint64
	rejected  atomic.Uint64
	requests  atomic.Uint64
}

// asyncBulkIndexer is a bounded single-worker implementation. It is safe for
// concurrent producers and concurrent Flush/Close calls.
type asyncBulkIndexer struct {
	client   *Client
	cfg      BulkConfig
	observer BulkObserver

	items   chan BulkItem
	flushes chan flushRequest
	closing chan struct{}
	stop    chan struct{}
	done    chan struct{}

	// admission protects the accepting transition and registration in
	// admissions only. Potentially blocking channel operations never hold it,
	// so Close can always publish closing and honor its own context.
	admission  sync.Mutex
	admissions sync.WaitGroup
	accepting  bool
	stopOnce   sync.Once

	resultMu sync.RWMutex
	closeErr error

	counters bulkCounters
}

var _ BulkIndexer = (*asyncBulkIndexer)(nil)

func newAsyncBulkIndexer(client *Client, cfg BulkConfig, observer BulkObserver) *asyncBulkIndexer {
	indexer := newDormantAsyncBulkIndexer(client, cfg, observer)
	go indexer.run()
	return indexer
}

func newManagedAsyncBulkIndexer(
	client *Client,
	cfg BulkConfig,
	observer BulkObserver,
	submit func(func(context.Context)) bool,
) (*asyncBulkIndexer, bool) {
	indexer := newDormantAsyncBulkIndexer(client, cfg, observer)
	if submit == nil || !submit(func(context.Context) { indexer.run() }) {
		return nil, false
	}
	return indexer, true
}

func newDormantAsyncBulkIndexer(client *Client, cfg BulkConfig, observer BulkObserver) *asyncBulkIndexer {
	return &asyncBulkIndexer{
		client:    client,
		cfg:       cfg,
		observer:  observer,
		items:     make(chan BulkItem, cfg.QueueCapacity),
		flushes:   make(chan flushRequest),
		closing:   make(chan struct{}),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		accepting: true,
	}
}

// Add validates, compacts, and copies an item before admission. Block mode
// honors ctx; reject mode returns ErrBulkQueueFull immediately when capacity
// is exhausted.
func (b *asyncBulkIndexer) beginAdmission() (<-chan struct{}, bool) {
	b.admission.Lock()
	defer b.admission.Unlock()
	if !b.accepting {
		return nil, false
	}
	b.admissions.Add(1)
	return b.closing, true
}

func (b *asyncBulkIndexer) endAdmission() { b.admissions.Done() }

// beginClose stops new admission immediately. A short coordinator waits only
// for operations that registered before that transition, then tells the worker
// to drain. Every registered operation observes closing, so this wait cannot be
// held hostage by a full queue.
func (b *asyncBulkIndexer) beginClose() {
	b.stopOnce.Do(func() {
		b.admission.Lock()
		b.accepting = false
		close(b.closing)
		b.admission.Unlock()
		go func() {
			b.admissions.Wait()
			close(b.stop)
		}()
	})
}

// Add validates, compacts, and copies an item before admission. Block mode
// honors both ctx and shutdown; reject mode returns ErrBulkQueueFull
// immediately when capacity is exhausted.
func (b *asyncBulkIndexer) Add(ctx context.Context, item BulkItem) error {
	if ctx == nil {
		return errors.New("elasticsearch: bulk Add requires a non-nil context")
	}
	normalized, err := normalizeBulkItem(item)
	if err != nil {
		return err
	}
	closing, admitted := b.beginAdmission()
	if !admitted {
		return ErrBulkClosed
	}
	defer b.endAdmission()

	if b.cfg.Backpressure == BackpressureReject {
		select {
		case <-closing:
			return ErrBulkClosed
		default:
		}
		select {
		case b.items <- normalized:
			b.counters.added.Add(1)
			return nil
		case <-closing:
			return ErrBulkClosed
		default:
			b.counters.rejected.Add(1)
			return ErrBulkQueueFull
		}
	}
	select {
	case b.items <- normalized:
		b.counters.added.Add(1)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-closing:
		return ErrBulkClosed
	case <-b.done:
		return ErrBulkClosed
	}
}

// Flush is a barrier for items whose Add completed before this call. It also
// reports failures from automatic flushes since the previous explicit barrier.
func (b *asyncBulkIndexer) Flush(ctx context.Context) error {
	if ctx == nil {
		return errors.New("elasticsearch: bulk Flush requires a non-nil context")
	}
	closing, admitted := b.beginAdmission()
	if !admitted {
		return ErrBulkClosed
	}
	defer b.endAdmission()

	request := flushRequest{done: make(chan error, 1)}
	select {
	case b.flushes <- request:
	case <-ctx.Done():
		return ctx.Err()
	case <-closing:
		return ErrBulkClosed
	case <-b.done:
		return ErrBulkClosed
	}
	select {
	case err := <-request.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-b.done:
		return ErrBulkClosed
	}
}

// Close atomically stops admission, drains every successfully accepted item,
// and flushes. Draining continues once started; each caller waits only for its
// own context and may call Close again with a fresh context to obtain the final
// result.
func (b *asyncBulkIndexer) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	b.beginClose()
	select {
	case <-b.done:
		b.resultMu.RLock()
		err := b.closeErr
		b.resultMu.RUnlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *asyncBulkIndexer) Stats() BulkStats {
	return BulkStats{
		Added:     b.counters.added.Load(),
		Flushed:   b.counters.flushed.Load(),
		Succeeded: b.counters.succeeded.Load(),
		Failed:    b.counters.failed.Load(),
		Rejected:  b.counters.rejected.Load(),
		Requests:  b.counters.requests.Load(),
	}
}

func (b *asyncBulkIndexer) run() {
	ticker := time.NewTicker(b.cfg.FlushInterval)
	defer ticker.Stop()
	defer func() {
		if recovered := recover(); recovered != nil {
			err := errors.New("elasticsearch: bulk worker panicked")
			b.resultMu.Lock()
			b.closeErr = errors.Join(b.closeErr, err)
			b.resultMu.Unlock()
			b.beginClose()
		}
		close(b.done)
	}()

	batch := make([]BulkItem, 0, b.cfg.FlushActions)
	batchBytes := 0
	var firstErr, pendingErr error
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := b.flushBatch(batch)
		if err != nil {
			firstErr = errors.Join(firstErr, err)
			pendingErr = errors.Join(pendingErr, err)
		}
		batch = batch[:0]
		batchBytes = 0
		return err
	}
	appendItem := func(item BulkItem) {
		batch = append(batch, item)
		batchBytes += estimateBulkItemBytes(item)
		if len(batch) >= b.cfg.FlushActions || batchBytes >= b.cfg.FlushBytes {
			_ = flush()
		}
	}
	drainItems := func() {
		for {
			select {
			case item := <-b.items:
				appendItem(item)
			default:
				return
			}
		}
	}

	for {
		select {
		case item := <-b.items:
			appendItem(item)
		case request := <-b.flushes:
			drainItems()
			_ = flush()
			request.done <- pendingErr
			pendingErr = nil
		case <-ticker.C:
			_ = flush()
		case <-b.stop:
			drainItems()
			_ = flush()
			b.resultMu.Lock()
			b.closeErr = firstErr
			b.resultMu.Unlock()
			return
		}
	}
}

const maxBulkResponseBytes int64 = 16 << 20

func (b *asyncBulkIndexer) flushBatch(items []BulkItem) error {
	body, err := encodeBulk(items)
	if err != nil {
		return errors.Join(err, b.observeBatchFailure(items, err))
	}
	b.counters.requests.Add(1)
	response, err := b.client.Do(context.Background(), Request{
		Method: http.MethodPost,
		Path:   "/_bulk",
		Body:   bytes.NewReader(body),
		Header: http.Header{
			"Content-Type": []string{"application/x-ndjson"},
			"Accept":       []string{"application/json"},
		},
	})
	if err != nil {
		return errors.Join(err, b.observeBatchFailure(items, err))
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		err := fmt.Errorf("elasticsearch: bulk request returned HTTP %d", response.StatusCode)
		return errors.Join(err, b.observeBatchFailure(items, err))
	}

	var payload struct {
		Items []map[string]struct {
			Status int `json:"status"`
			Error  *struct {
				Type   string `json:"type"`
				Reason string `json:"reason"`
			} `json:"error"`
		} `json:"items"`
	}
	limited := &io.LimitedReader{R: response.Body, N: maxBulkResponseBytes + 1}
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(&payload); err != nil {
		if limited.N == 0 {
			err = fmt.Errorf("elasticsearch: bulk response exceeds %d bytes", maxBulkResponseBytes)
		} else {
			err = fmt.Errorf("elasticsearch: decode bulk response: %w", err)
		}
		return errors.Join(err, b.observeBatchFailure(items, err))
	}
	var trailing any
	trailingErr := decoder.Decode(&trailing)
	if limited.N == 0 {
		err = fmt.Errorf("elasticsearch: bulk response exceeds %d bytes", maxBulkResponseBytes)
		return errors.Join(err, b.observeBatchFailure(items, err))
	}
	if !errors.Is(trailingErr, io.EOF) {
		if trailingErr == nil {
			err = errors.New("elasticsearch: bulk response contains multiple JSON values")
		} else {
			err = fmt.Errorf("elasticsearch: decode trailing bulk response: %w", trailingErr)
		}
		return errors.Join(err, b.observeBatchFailure(items, err))
	}
	if len(payload.Items) != len(items) {
		err := fmt.Errorf("elasticsearch: bulk response item count %d does not match request count %d", len(payload.Items), len(items))
		return errors.Join(err, b.observeBatchFailure(items, err))
	}

	var firstErr error
	for i, item := range items {
		entry, ok := payload.Items[i][string(item.Operation)]
		result := BulkResult{Item: cloneBulkItem(item)}
		if !ok {
			result.Error = fmt.Errorf("elasticsearch: bulk response item %d has no %q result", i, item.Operation)
			b.counters.failed.Add(1)
			firstErr = errors.Join(firstErr, result.Error)
		} else {
			result.Status = entry.Status
			if entry.Error != nil {
				result.ErrorType = entry.Error.Type
				result.ErrorReason = entry.Error.Reason
			}
			if entry.Error != nil || entry.Status < 200 || entry.Status >= 300 {
				result.Error = &BulkItemError{
					Operation: item.Operation,
					Index:     item.Index,
					ID:        item.ID,
					Status:    entry.Status,
					Type:      result.ErrorType,
					Reason:    result.ErrorReason,
				}
				b.counters.failed.Add(1)
				firstErr = errors.Join(firstErr, result.Error)
			} else {
				b.counters.succeeded.Add(1)
			}
		}
		b.counters.flushed.Add(1)
		firstErr = errors.Join(firstErr, b.observe(result))
	}
	return firstErr
}

type BulkItemError struct {
	Operation BulkOperation
	Index     string
	ID        string
	Status    int
	Type      string
	Reason    string
}

func (e *BulkItemError) Error() string {
	return fmt.Sprintf("elasticsearch: bulk %s %s/%s failed with HTTP %d (%s: %s)", e.Operation, e.Index, e.ID, e.Status, e.Type, e.Reason)
}

func (b *asyncBulkIndexer) observeBatchFailure(items []BulkItem, err error) error {
	var observerErr error
	for _, item := range items {
		b.counters.failed.Add(1)
		b.counters.flushed.Add(1)
		observerErr = errors.Join(observerErr, b.observe(BulkResult{Item: cloneBulkItem(item), Error: err}))
	}
	return observerErr
}

func (b *asyncBulkIndexer) observe(result BulkResult) error {
	var observerErr error
	if result.Item.Observer != nil {
		observerErr = errors.Join(observerErr, callBulkObserver("item", result.Item.Observer, result))
	}
	if b.observer != nil {
		observerErr = errors.Join(observerErr, callBulkObserver("global", b.observer, result))
	}
	return observerErr
}

func callBulkObserver(kind string, observer BulkObserver, result BulkResult) (err error) {
	defer func() {
		if recover() != nil {
			err = fmt.Errorf("elasticsearch: %s bulk observer panicked", kind)
		}
	}()
	observer.Observe(result)
	return nil
}

func normalizeBulkItem(item BulkItem) (BulkItem, error) {
	item.Index = strings.TrimSpace(item.Index)
	if item.Index == "" {
		return BulkItem{}, errors.New("elasticsearch: bulk item index is required")
	}
	if item.Operation == "" {
		item.Operation = BulkIndex
	}
	switch item.Operation {
	case BulkIndex, BulkCreate, BulkUpdate:
		if len(item.Document) == 0 || !json.Valid(item.Document) {
			return BulkItem{}, fmt.Errorf("elasticsearch: bulk %s document must be valid JSON", item.Operation)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, item.Document); err != nil {
			return BulkItem{}, fmt.Errorf("elasticsearch: compact bulk document: %w", err)
		}
		item.Document = append([]byte(nil), compact.Bytes()...)
	case BulkDelete:
		if len(item.Document) != 0 {
			return BulkItem{}, errors.New("elasticsearch: bulk delete document must be empty")
		}
	default:
		return BulkItem{}, fmt.Errorf("elasticsearch: unsupported bulk operation %q", item.Operation)
	}
	return item, nil
}

func cloneBulkItem(item BulkItem) BulkItem {
	item.Document = append([]byte(nil), item.Document...)
	return item
}

func estimateBulkItemBytes(item BulkItem) int {
	return len(item.Index) + len(item.ID) + len(item.Routing) + len(item.Document) + 96
}

func encodeBulk(items []BulkItem) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	for _, item := range items {
		metadata := map[string]string{"_index": item.Index}
		if item.ID != "" {
			metadata["_id"] = item.ID
		}
		if item.Routing != "" {
			metadata["routing"] = item.Routing
		}
		if err := encoder.Encode(map[string]any{string(item.Operation): metadata}); err != nil {
			return nil, err
		}
		if item.Operation != BulkDelete {
			output.Write(item.Document)
			output.WriteByte('\n')
		}
	}
	return output.Bytes(), nil
}
