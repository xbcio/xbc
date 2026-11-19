package requestid

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
	"github.com/xbcio/xbc/transport/web/extensions/observability/accesslog"
)

// loggedRecord is one record captured by recordingLogger, including the fixed
// fields a With call derived.
type loggedRecord struct {
	message string
	fields  []any
}

// recordingLogger is a facade Logger that keeps every record in memory. It is
// deliberately dependency-free: the middleware only needs the log.Logger
// contract, and the web module must not import a logging backend.
type recordingLogger struct {
	mu      *sync.Mutex
	records *[]loggedRecord
	fields  []any
}

func newRecordingLogger() *recordingLogger {
	return &recordingLogger{mu: new(sync.Mutex), records: new([]loggedRecord)}
}

func (l *recordingLogger) record(message string, kv ...any) {
	fields := make([]any, 0, len(l.fields)+len(kv))
	fields = append(fields, l.fields...)
	fields = append(fields, kv...)
	l.mu.Lock()
	defer l.mu.Unlock()
	*l.records = append(*l.records, loggedRecord{message: message, fields: fields})
}

func (l *recordingLogger) Debug(message string, kv ...any) { l.record(message, kv...) }
func (l *recordingLogger) Info(message string, kv ...any)  { l.record(message, kv...) }
func (l *recordingLogger) Warn(message string, kv ...any)  { l.record(message, kv...) }
func (l *recordingLogger) Error(message string, kv ...any) { l.record(message, kv...) }

// Fatal records without terminating: no test reaches it, and a logger that
// exits would make a failing assertion unreportable.
func (l *recordingLogger) Fatal(message string, kv ...any) { l.record(message, kv...) }

func (l *recordingLogger) With(kv ...any) log.Logger {
	fields := make([]any, 0, len(l.fields)+len(kv))
	fields = append(fields, l.fields...)
	fields = append(fields, kv...)
	return &recordingLogger{mu: l.mu, records: l.records, fields: fields}
}

func (l *recordingLogger) Enabled(log.Level) bool { return true }

// values returns every value recorded for key on records with the given
// message, preserving duplicates so a test can prove a field was not stacked.
func (l *recordingLogger) values(message, key string) []any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var values []any
	for _, record := range *l.records {
		if record.message != message {
			continue
		}
		for i := 0; i+1 < len(record.fields); i += 2 {
			name, ok := record.fields[i].(string)
			if ok && name == key {
				values = append(values, record.fields[i+1])
			}
		}
	}
	return values
}

// bindLogger is an upstream middleware that makes logger the request-scoped
// logger the requestid middleware enriches.
func bindLogger(logger log.Logger) web.Handler {
	return func(_ context.Context, c *web.Ctx) error {
		c.SetContext(log.NewContext(c.Request().Context(), logger))
		c.Next()
		return nil
	}
}

// provideTrace is an upstream middleware standing in for HTTP tracing: it
// leaves a trace on the request context without binding a logger, which is the
// shape the tracing extension produces.
func provideTrace(_ context.Context, c *web.Ctx) error {
	c.SetContext(log.WithTrace(c.Request().Context(), log.NewTrace("test request")))
	c.Next()
	return nil
}

func logHandler(message string) web.Handler {
	return func(_ context.Context, c *web.Ctx) error {
		log.Ctx(c.Request().Context()).Info(message)
		c.String(http.StatusOK, "ok")
		return nil
	}
}

func TestBindsRequestIDToRequestLogger(t *testing.T) {
	for _, test := range []struct {
		name         string
		inbound      string
		acceptsInput bool
	}{
		{name: "accepted inbound id", inbound: "client-123", acceptsInput: true},
		{name: "regenerated invalid id", inbound: "bad id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := newRecordingLogger()
			router := enginetest.New()
			router.Use(bindLogger(recorder), New().handle)
			router.GET("/", logHandler("business log"))

			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set(defaultHeader, test.inbound)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			id := response.Header().Get(defaultHeader)
			if id == "" {
				t.Fatal("the response carries no request ID")
			}
			if test.acceptsInput && id != test.inbound {
				t.Fatalf("response ID = %q, want the accepted inbound ID %q", id, test.inbound)
			}
			if !test.acceptsInput && (id == test.inbound || len(id) != 32) {
				t.Fatalf("response ID = %q, want a fresh 32-character ID", id)
			}
			values := recorder.values("business log", log.RequestIDField)
			if len(values) != 1 || values[0] != id {
				t.Fatalf("logged request_id = %v, want exactly the response ID %q", values, id)
			}
		})
	}
}

func TestBindLoggerDisabledLeavesRequestLoggerUntouched(t *testing.T) {
	recorder := newRecordingLogger()
	cfg := DefaultConfig()
	cfg.BindLogger = false
	p, err := newPlugin(cfg)
	if err != nil {
		t.Fatalf("newPlugin() error = %v", err)
	}

	router := enginetest.New()
	router.Use(bindLogger(recorder), p.handle)
	router.GET("/", logHandler("business log"))

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

	if id := response.Header().Get(defaultHeader); id == "" {
		t.Fatal("bind_logger: false must not stop the ID from being published")
	}
	if values := recorder.values("business log", log.RequestIDField); len(values) != 0 {
		t.Fatalf("logged request_id = %v, want none with bind_logger disabled", values)
	}
}

func TestRequestIDLogBindingIsIdempotent(t *testing.T) {
	recorder := newRecordingLogger()
	p := New()

	router := enginetest.New()
	// The same middleware twice stands in for a chain that runs one request
	// through the plugin more than once.
	router.Use(bindLogger(recorder), p.handle, p.handle)
	router.GET("/", logHandler("business log"))

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

	id := response.Header().Get(defaultHeader)
	values := recorder.values("business log", log.RequestIDField)
	if len(values) != 1 || values[0] != id {
		t.Fatalf("logged request_id = %v, want exactly one value equal to the response ID %q", values, id)
	}
}

func TestRequestIDStaysBoundAcrossAPanic(t *testing.T) {
	recorder := newRecordingLogger()
	router := enginetest.New()
	router.Use(bindLogger(recorder), New().handle)
	router.GET("/", func(_ context.Context, c *web.Ctx) error {
		defer func() {
			if recover() == nil {
				t.Error("the handler did not panic")
				return
			}
			log.Ctx(c.Request().Context()).Info("recovered")
			c.String(http.StatusInternalServerError, "recovered")
		}()
		panic("boom")
	})

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

	id := response.Header().Get(defaultHeader)
	values := recorder.values("recovered", log.RequestIDField)
	if id == "" || len(values) != 1 || values[0] != id {
		t.Fatalf("logged request_id = %v, want exactly the response ID %q", values, id)
	}
}

func TestTraceFieldsAndRequestIDStaySeparate(t *testing.T) {
	recorder := newRecordingLogger()
	previous := log.L()
	log.SetLogger(recorder)
	t.Cleanup(func() { log.SetLogger(previous) })

	var traceID, traceRequestID string
	router := enginetest.New()
	router.Use(provideTrace, New().handle)
	router.GET("/", func(_ context.Context, c *web.Ctx) error {
		trace := log.TraceFrom(c.Request().Context())
		traceID = trace.TraceID().String()
		traceRequestID = trace.RequestID
		log.Ctx(c.Request().Context()).Info("business log")
		c.String(http.StatusOK, "ok")
		return nil
	})

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

	id := response.Header().Get(defaultHeader)
	if traceRequestID != id {
		t.Fatalf("trace request_id = %q, want the request ID %q", traceRequestID, id)
	}
	requestIDs := recorder.values("business log", log.RequestIDField)
	if len(requestIDs) != 1 || requestIDs[0] != id {
		t.Fatalf("logged request_id = %v, want exactly one value equal to %q", requestIDs, id)
	}
	traceIDs := recorder.values("business log", "trace_id")
	if len(traceIDs) != 1 || traceIDs[0] != traceID || traceID == id {
		t.Fatalf("logged trace_id = %v (want %q), distinct from request_id %q", traceIDs, traceID, id)
	}
}

func TestAccessLogRecordsTheSameRequestID(t *testing.T) {
	recorder := newRecordingLogger()
	previous := log.L()
	log.SetLogger(recorder)
	t.Cleanup(func() { log.SetLogger(previous) })

	router := enginetest.New()
	router.Use(New().handle, accesslog.New().Handler())
	router.GET("/", logHandler("business log"))

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

	id := response.Header().Get(defaultHeader)
	business := recorder.values("business log", log.RequestIDField)
	access := recorder.values("http request completed", log.RequestIDField)
	if id == "" || len(business) != 1 || len(access) != 1 || business[0] != id || access[0] != id {
		t.Fatalf("business request_id = %v, access request_id = %v, response ID = %q; want one shared value", business, access, id)
	}
}
