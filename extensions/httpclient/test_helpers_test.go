package httpclient

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/xbcio/xbc/extensions/observability/metrics"
	"github.com/xbcio/xbc/extensions/observability/tracing"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// recordedLine is one captured log entry.
type recordedLine struct {
	Level string
	Msg   string
	KV    []any
}

// recordingLogger is a log.Logger that keeps what it was told, so tests can
// grep the exact text a component would have emitted.
type recordingLogger struct {
	mu    sync.Mutex
	lines []recordedLine
}

func (l *recordingLogger) record(level, msg string, kv []any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, recordedLine{Level: level, Msg: msg, KV: append([]any(nil), kv...)})
}

func (l *recordingLogger) Debug(msg string, kv ...any) { l.record("DEBUG", msg, kv) }
func (l *recordingLogger) Info(msg string, kv ...any)  { l.record("INFO", msg, kv) }
func (l *recordingLogger) Warn(msg string, kv ...any)  { l.record("WARN", msg, kv) }
func (l *recordingLogger) Error(msg string, kv ...any) { l.record("ERROR", msg, kv) }
func (l *recordingLogger) Fatal(msg string, kv ...any) { l.record("FATAL", msg, kv) }
func (l *recordingLogger) With(_ ...any) log.Logger    { return l }
func (l *recordingLogger) Enabled(log.Level) bool      { return true }

var _ log.Logger = (*recordingLogger)(nil)

// text renders every captured line for a substring assertion.
func (l *recordingLogger) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var builder strings.Builder
	for _, line := range l.lines {
		fmt.Fprintf(&builder, "%s %s %v\n", line.Level, line.Msg, line.KV)
	}
	return builder.String()
}

// testHost is the RuntimeHost half of a plugin.Context a test builds directly.
// It supplies only what an endpoint report reads: a logger.
type testHost struct {
	execution context.Context
	logger    log.Logger
}

func (h *testHost) ExecutionContext() context.Context                            { return h.execution }
func (h *testHost) Logger() log.Logger                                           { return h.logger }
func (h *testHost) ProcessInstance() string                                      { return "httpclient-test-process" }
func (h *testHost) Service() plugin.ServiceIdentity                              { return plugin.ServiceIdentity{} }
func (h *testHost) IngressStopBudget() time.Duration                             { return 0 }
func (h *testHost) TrafficGate() <-chan struct{}                                 { return nil }
func (h *testHost) SubmitTask(plugin.Identity, func(context.Context), bool) bool { return false }
func (h *testHost) RequestShutdown(plugin.Identity, string) bool                 { return false }
func (h *testHost) Admission(plugin.Identity) plugin.Admission                   { return nil }
func (h *testHost) AdmissionFor(plugin.Identity, plugin.WorkloadKey) plugin.Admission {
	return nil
}

func newTestContext(logger log.Logger, instance string) *plugin.Context {
	return plugin.NewRuntimeContext(
		&testHost{execution: context.Background(), logger: logger},
		plugin.Identity{Plugin: Key, Instance: instance},
	)
}

// mustNormalize runs the production preparation over the literal a test
// wrote, so a test config reaches newClient exactly as a bound one would.
func mustNormalize(t *testing.T, config Config) Config {
	t.Helper()
	normalized, err := config.normalized()
	if err != nil {
		t.Fatalf("normalized(%+v) error = %v", config, err)
	}
	return normalized
}

// newTestClient builds a client through the production constructor, with no
// tracing and no metrics unless the test supplies them.
func newTestClient(t *testing.T, config Config, tracer tracing.Handle, registerer metrics.Registerer) *Client {
	t.Helper()
	client, err := newClient("test", mustNormalize(t, config), tracer, registerer)
	if err != nil {
		t.Fatalf("newClient error = %v", err)
	}
	t.Cleanup(func() { _ = client.stop(context.Background()) })
	return client
}

// newSyntheticClient builds a client whose next transport is the test's, so
// retry decisions can be driven without a network. Everything else is the
// production shape.
func newSyntheticClient(t *testing.T, config Config, transport http.RoundTripper) *Client {
	t.Helper()
	normalized := mustNormalize(t, config)
	client := &Client{instance: "test", config: normalized}
	engine := &retryTransport{config: normalized, next: transport, stopped: &client.stopped}
	client.client = &http.Client{Transport: engine, Timeout: normalized.TotalTimeout}
	t.Cleanup(func() { _ = client.stop(context.Background()) })
	return client
}

func mustRequest(t *testing.T, method, url string, body io.Reader) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("http.NewRequest(%s, %s) error = %v", method, url, err)
	}
	return request
}

// trackingBody records whether and how much it was read and whether it was
// closed, which is how the "bodies are closed before a retry" behavior is
// pinned.
type trackingBody struct {
	reader *strings.Reader
	closed atomic.Bool
	reads  atomic.Int64
}

func newTrackingBody(content string) *trackingBody {
	return &trackingBody{reader: strings.NewReader(content)}
}

func (b *trackingBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	return b.reader.Read(p)
}

func (b *trackingBody) Close() error {
	b.closed.Store(true)
	return nil
}

// syntheticOutcome is one scripted attempt result. The last outcome repeats
// for every further attempt.
type syntheticOutcome struct {
	status int
	header http.Header
	err    error
}

func statusOutcome(status int) syntheticOutcome {
	return syntheticOutcome{status: status, header: make(http.Header)}
}

// syntheticTransport serves scripted outcomes and keeps every body it handed
// out, so a test can assert what a retry did to the previous response.
type syntheticTransport struct {
	mu       sync.Mutex
	outcomes []syntheticOutcome
	bodies   []*trackingBody
	attempts int
}

func newSyntheticTransport(outcomes ...syntheticOutcome) *syntheticTransport {
	return &syntheticTransport{outcomes: outcomes}
}

func (s *syntheticTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	index := s.attempts
	s.attempts++
	s.mu.Unlock()
	if index >= len(s.outcomes) {
		index = len(s.outcomes) - 1
	}
	outcome := s.outcomes[index]
	if outcome.err != nil {
		s.mu.Lock()
		s.bodies = append(s.bodies, nil)
		s.mu.Unlock()
		return nil, outcome.err
	}
	body := newTrackingBody("attempt-body-" + strconv.Itoa(index))
	s.mu.Lock()
	s.bodies = append(s.bodies, body)
	s.mu.Unlock()
	return &http.Response{
		StatusCode: outcome.status,
		Header:     outcome.header,
		Body:       body,
		Request:    req,
	}, nil
}

func (s *syntheticTransport) attemptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

func (s *syntheticTransport) body(index int) *trackingBody {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[index]
}

// observedConnStates records the connection states one test server saw, so a
// test can wait for a transition instead of sleeping on a guess. Client-side
// pools are private, so the peer holding the other end is the only witness a
// "the idle connection was closed" assertion can honestly use.
type observedConnStates struct {
	mu     sync.Mutex
	states map[http.ConnState]int
}

func newObservedConnStates() *observedConnStates {
	return &observedConnStates{states: make(map[http.ConnState]int)}
}

func (o *observedConnStates) record(_ net.Conn, state http.ConnState) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.states[state]++
}

func (o *observedConnStates) saw(state http.ConnState) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.states[state] > 0
}

// scriptedServer answers every request with the scripted statuses in order;
// the last status repeats. It records each request's headers and body so the
// wire behavior -- traceparent, replayed bodies -- can be asserted.
type scriptedServer struct {
	*httptest.Server

	mu              sync.Mutex
	statuses        []int
	responseHeaders map[int]http.Header
	calls           int
	headers         []http.Header
	bodies          []string
}

func newScriptedServer(t *testing.T, statuses ...int) *scriptedServer {
	t.Helper()
	server := &scriptedServer{statuses: statuses, responseHeaders: make(map[int]http.Header)}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		server.mu.Lock()
		index := server.calls
		if index >= len(server.statuses) {
			index = len(server.statuses) - 1
		}
		status := server.statuses[index]
		for key, values := range server.responseHeaders[index] {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		server.calls++
		server.headers = append(server.headers, r.Header.Clone())
		server.bodies = append(server.bodies, string(body))
		server.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(http.StatusText(status)))
	}))
	t.Cleanup(server.Close)
	return server
}

// setResponseHeader attaches a header to the reply of one scripted call.
func (s *scriptedServer) setResponseHeader(index int, key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.responseHeaders[index] == nil {
		s.responseHeaders[index] = make(http.Header)
	}
	s.responseHeaders[index].Add(key, value)
}

func (s *scriptedServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *scriptedServer) header(index int) http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headers[index]
}

func (s *scriptedServer) body(index int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[index]
}

// fakeTracingHandle is the Handle a test composes in place of the tracing
// plugin: a real SDK provider behind an in-memory recorder, plus the W3C
// propagator, so spans and injected headers are the real article.
type fakeTracingHandle struct {
	provider   *sdktrace.TracerProvider
	recorder   *tracetest.SpanRecorder
	propagator propagation.TraceContext

	mu      sync.Mutex
	injects int
}

func newFakeTracingHandle() *fakeTracingHandle {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	return &fakeTracingHandle{provider: provider, recorder: recorder}
}

func (h *fakeTracingHandle) Provider() trace.TracerProvider { return h.provider }

func (h *fakeTracingHandle) Tracer(name string, options ...trace.TracerOption) trace.Tracer {
	return h.provider.Tracer(name, options...)
}

func (h *fakeTracingHandle) Extract(ctx context.Context, carrier propagation.TextMapCarrier) context.Context {
	return h.propagator.Extract(ctx, carrier)
}

func (h *fakeTracingHandle) Inject(ctx context.Context, carrier propagation.TextMapCarrier) {
	h.mu.Lock()
	h.injects++
	h.mu.Unlock()
	h.propagator.Inject(ctx, carrier)
}

func (h *fakeTracingHandle) ForceFlush(context.Context) error { return nil }
func (h *fakeTracingHandle) Shutdown(context.Context) error   { return nil }

func (h *fakeTracingHandle) injectCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.injects
}

var _ tracing.Handle = (*fakeTracingHandle)(nil)

// fakeRegisterer is the metrics.Registerer a test composes in place of the
// metrics plugin. Register forwards to a real registry -- so Gather and
// label-set assertions see exactly what a scrape would -- and counts the
// calls, so all-or-nothing registration is observable.
type fakeRegisterer struct {
	inner *prometheus.Registry

	mu          sync.Mutex
	batches     [][]prometheus.Collector
	registerErr error
}

func newFakeRegisterer() *fakeRegisterer {
	return &fakeRegisterer{inner: prometheus.NewRegistry()}
}

func (r *fakeRegisterer) Register(collectors ...prometheus.Collector) error {
	r.mu.Lock()
	r.batches = append(r.batches, append([]prometheus.Collector(nil), collectors...))
	err := r.registerErr
	r.mu.Unlock()
	if err != nil {
		return err
	}
	// Mirror the all-or-nothing shape of metrics.Registry.Register: a
	// failure removes what this call already added.
	registered := make([]prometheus.Collector, 0, len(collectors))
	for _, collector := range collectors {
		if err := r.inner.Register(collector); err != nil {
			for _, previous := range registered {
				r.inner.Unregister(previous)
			}
			return err
		}
		registered = append(registered, collector)
	}
	return nil
}

func (r *fakeRegisterer) Unregister(collector prometheus.Collector) bool {
	return r.inner.Unregister(collector)
}

func (r *fakeRegisterer) batchCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.batches)
}

func (r *fakeRegisterer) lastBatchSize() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.batches) == 0 {
		return 0
	}
	return len(r.batches[len(r.batches)-1])
}

var _ metrics.Registerer = (*fakeRegisterer)(nil)
