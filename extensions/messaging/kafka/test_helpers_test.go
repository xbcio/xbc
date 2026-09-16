package kafka

import (
	"context"
	"errors"
	"sync"
	"time"

	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

type fakeHost struct {
	mu        sync.RWMutex
	accepting bool
	gate      chan struct{}
	gateOnce  sync.Once

	taskCtx        context.Context
	cancel         context.CancelFunc
	tasks          sync.WaitGroup
	taskMu         sync.Mutex
	count          int
	critical       int
	admissionLimit int
}

func newFakeHost() *fakeHost {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeHost{
		accepting:      true,
		admissionLimit: -1,
		gate:           make(chan struct{}),
		taskCtx:        ctx,
		cancel:         cancel,
	}
}

func (h *fakeHost) ExecutionContext() context.Context { return h.taskCtx }
func (*fakeHost) Logger() corelog.Logger              { return corelog.Nop() }
func (h *fakeHost) TrafficGate() <-chan struct{}      { return h.gate }

func (h *fakeHost) SubmitTask(_ plugin.Identity, fn func(context.Context), critical bool) bool {
	h.mu.Lock()
	h.taskMu.Lock()
	if !h.accepting || fn == nil || h.admissionLimit >= 0 && h.count >= h.admissionLimit {
		h.taskMu.Unlock()
		h.mu.Unlock()
		return false
	}
	h.count++
	if critical {
		h.critical++
	}
	h.tasks.Add(1)
	h.taskMu.Unlock()
	h.mu.Unlock()
	go func() {
		defer h.tasks.Done()
		fn(h.taskCtx)
	}()
	return true
}

func (*fakeHost) RequestShutdown(plugin.Identity, string) bool { return true }

func (h *fakeHost) setAccepting(value bool) {
	h.mu.Lock()
	h.accepting = value
	h.mu.Unlock()
}

func (h *fakeHost) setAdmissionLimit(limit int) {
	h.mu.Lock()
	h.admissionLimit = limit
	h.mu.Unlock()
}

func (h *fakeHost) openTraffic() { h.gateOnce.Do(func() { close(h.gate) }) }

func (h *fakeHost) taskCounts() (int, int) {
	h.taskMu.Lock()
	defer h.taskMu.Unlock()
	return h.count, h.critical
}

func (h *fakeHost) taskCount() int {
	count, _ := h.taskCounts()
	return count
}

func (h *fakeHost) close() {
	h.cancel()
	h.tasks.Wait()
}

func runtimeContext(host plugin.RuntimeHost, instance string) *plugin.Context {
	return plugin.NewRuntimeContext(host, plugin.Identity{Plugin: Key, Instance: instance})
}

func managedClient(t testingT, cfg Config, factory backendFactory) *Client {
	t.Helper()
	client, err := newManagedClient(cfg, factory)
	if err != nil {
		t.Fatalf("newManagedClient() error = %v", err)
	}
	return client
}

type fakeWriter struct {
	mu sync.Mutex

	writes     [][]Message
	writeErr   error
	pingErr    error
	pingCalls  int
	closeErr   error
	active     int
	maxActive  int
	closeCount int

	writeBlock   <-chan struct{}
	writeStarted chan struct{}
	writeOnce    sync.Once
	closeBlock   <-chan struct{}
	closeStarted chan struct{}
	closeOnce    sync.Once
	pingBlock    <-chan struct{}
}

func (w *fakeWriter) Ping(ctx context.Context) error {
	w.mu.Lock()
	w.pingCalls++
	block := w.pingBlock
	err := w.pingErr
	w.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (w *fakeWriter) pings() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pingCalls
}

func (w *fakeWriter) Write(ctx context.Context, messages []Message) error {
	w.mu.Lock()
	w.active++
	if w.active > w.maxActive {
		w.maxActive = w.active
	}
	block := w.writeBlock
	started := w.writeStarted
	err := w.writeErr
	w.mu.Unlock()
	if started != nil {
		w.writeOnce.Do(func() { close(started) })
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	w.mu.Lock()
	w.active--
	if err == nil {
		copied := make([]Message, len(messages))
		for i, message := range messages {
			copied[i] = cloneMessage(message)
		}
		w.writes = append(w.writes, copied)
	}
	w.mu.Unlock()
	return err
}

func (w *fakeWriter) Close() error {
	w.mu.Lock()
	w.closeCount++
	block := w.closeBlock
	started := w.closeStarted
	err := w.closeErr
	w.mu.Unlock()
	if started != nil {
		w.closeOnce.Do(func() { close(started) })
	}
	if block != nil {
		<-block
	}
	return err
}

func cloneMessage(message Message) Message {
	message.Key = append([]byte(nil), message.Key...)
	message.Value = append([]byte(nil), message.Value...)
	message.Headers = append([]Header(nil), message.Headers...)
	for i := range message.Headers {
		message.Headers[i].Value = append([]byte(nil), message.Headers[i].Value...)
	}
	return message
}

type fetchResult struct {
	message fetchedMessage
	err     error
}

type fakeReader struct {
	queue  chan fetchResult
	closed chan struct{}
	once   sync.Once

	mu             sync.Mutex
	fetchCalls     int
	commitCalls    int
	commits        []fetchedMessage
	commitFailures int
	commitErr      error
	closeCount     int
	closeErr       error
}

func newFakeReader() *fakeReader {
	return &fakeReader{queue: make(chan fetchResult, 32), closed: make(chan struct{})}
}

func (r *fakeReader) Fetch(ctx context.Context) (fetchedMessage, error) {
	r.mu.Lock()
	r.fetchCalls++
	r.mu.Unlock()
	select {
	case result := <-r.queue:
		return result.message, result.err
	case <-r.closed:
		return fetchedMessage{}, errors.New("reader closed")
	case <-ctx.Done():
		return fetchedMessage{}, ctx.Err()
	}
}

func (r *fakeReader) Commit(ctx context.Context, message fetchedMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commitCalls++
	if r.commitFailures > 0 {
		r.commitFailures--
		if r.commitErr != nil {
			return r.commitErr
		}
		return errors.New("commit failed")
	}
	r.commits = append(r.commits, message)
	return nil
}

func (r *fakeReader) Close() error {
	r.once.Do(func() {
		r.mu.Lock()
		r.closeCount++
		r.mu.Unlock()
		close(r.closed)
	})
	return r.closeErr
}

func (r *fakeReader) enqueue(message Message) {
	r.queue <- fetchResult{message: fetchedMessage{message: cloneMessage(message), raw: message.Offset}}
}

func (r *fakeReader) enqueueError(err error) { r.queue <- fetchResult{err: err} }

type fakeFactory struct {
	mu sync.Mutex

	writer    messageWriter
	writerErr error
	readers   map[string]messageReader
	readerErr map[string]error

	writerConfig normalizedConfig
	readerOrder  []string
}

func (f *fakeFactory) NewWriter(cfg normalizedConfig) (messageWriter, error) {
	f.mu.Lock()
	f.writerConfig = cfg
	f.mu.Unlock()
	return f.writer, f.writerErr
}

func (f *fakeFactory) NewReader(_ normalizedConfig, name string, _ ConsumerConfig) (messageReader, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readerOrder = append(f.readerOrder, name)
	return f.readers[name], f.readerErr[name]
}

func validConfig() Config {
	return Config{Brokers: []string{"127.0.0.1:9092"}}
}

func consumerConfig(policy string) ConsumerConfig {
	return ConsumerConfig{
		GroupID:             "workers",
		Topics:              []string{"jobs"},
		HandlerMaxAttempts:  3,
		HandlerRetryBackoff: time.Millisecond,
		FetchErrorBackoff:   time.Millisecond,
		ErrorPolicy:         policy,
	}
}

func waitFor(t testingT, condition func() bool, message string) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	if !condition() {
		t.Fatalf("timeout waiting for %s", message)
	}
}

type testingT interface {
	Helper()
	Fatalf(string, ...any)
}
