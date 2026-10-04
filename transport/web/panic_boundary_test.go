package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// panicLogger captures every Warn and Error entry's message and fields, so a
// test can assert on exactly what the panic boundary recorded without
// inspecting its unexported state.
type panicLogger struct {
	log.Logger

	mu      sync.Mutex
	entries []panicLogEntry
}

type panicLogEntry struct {
	level  string
	msg    string
	fields []any
}

func (l *panicLogger) Warn(msg string, kv ...any)  { l.add("warn", msg, kv...) }
func (l *panicLogger) Error(msg string, kv ...any) { l.add("error", msg, kv...) }

func (l *panicLogger) add(level, msg string, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, panicLogEntry{level: level, msg: msg, fields: append([]any(nil), kv...)})
}

func (l *panicLogger) snapshot() []panicLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]panicLogEntry(nil), l.entries...)
}

// fieldValue returns the value recorded under key, or nil if the entry never
// recorded that key.
func fieldValue(entry panicLogEntry, key string) any {
	for i := 0; i+1 < len(entry.fields); i += 2 {
		if entry.fields[i] == key {
			return entry.fields[i+1]
		}
	}
	return nil
}

func hasField(entry panicLogEntry, key string) bool {
	for i := 0; i+1 < len(entry.fields); i += 2 {
		if entry.fields[i] == key {
			return true
		}
	}
	return false
}

// startPanicServer wires a fresh Server with logger installed on the fake
// host before Start, so the panic boundary's log entries are observable.
func startPanicServer(t *testing.T, stack bool, routes ...plugin.Entry[web.RouteContributor]) (*web.Server, *panicLogger) {
	t.Helper()
	return startPanicServerWith(t, stack, serverInputs{routes: routes})
}

// startPanicServerWith is startPanicServer with the full input set, for tests
// that need to contribute something besides routes -- an ErrorMapper being the
// case that matters: the panic boundary's broken-connection path must abort
// without ever handing the panic to the error boundary.
func startPanicServerWith(t *testing.T, stack bool, inputs serverInputs) (*web.Server, *panicLogger) {
	t.Helper()
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.Recovery.Stack = stack

	logger := &panicLogger{Logger: log.Nop()}
	server, ctx, host := newPingServer(t, cfg, inputs)
	host.logger = logger
	require.NoError(t, server.Start(ctx))
	// CurrentRoute (and therefore the "route" log field) only resolves once
	// the route table has been frozen, which OpenTraffic does; Start alone
	// binds the listener and assembles the chain.
	require.NoError(t, server.OpenTraffic(ctx))
	return server, logger
}

// TestPanicBoundaryRecoversAndRendersInternalServerError pins the core
// contract: an uncaught handler panic becomes a safe 500 Problem Detail
// instead of escaping to the engine, the response carries none of the headers
// the panicking handler had already set, and neither the panic value nor
// anything else from the request -- query string, Authorization header -- is
// written to the log.
func TestPanicBoundaryRecoversAndRendersInternalServerError(t *testing.T) {
	server, logger := startPanicServer(t, true, plugin.Entry[web.RouteContributor]{
		Identity: plugin.Identity{Plugin: "panictest"},
		Value: fakeRouteContributor{register: func(router *web.Router) {
			router.GET("/boom", func(_ context.Context, c *web.Ctx) error {
				// A handler can leave unsafe headers behind before panicking;
				// the boundary's own response must not carry any of them.
				c.SetHeader("Content-Encoding", "br")
				c.SetHeader("Content-Length", "999")
				c.SetHeader("Content-Type", "text/plain")
				panic("secret-token-value")
			}).Name("boom.route")
		}},
	})
	engine := testEngineOf(t, server)

	request := httptest.NewRequest(http.MethodGet, "/boom?password=also-secret", nil)
	request.Header.Set("Authorization", "Bearer very-secret")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	assert.Equal(t, http.StatusInternalServerError, response.Code)
	var problem web.ProblemDetail
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
	assert.Equal(t, http.StatusInternalServerError, problem.Status)
	assert.Equal(t, "internal_server_error", problem.Properties["code"])
	assert.Equal(t, "/boom", problem.Instance)
	assert.Empty(t, response.Header().Get("Content-Encoding"),
		"a handler-set Content-Encoding must not describe a body the boundary replaced")
	assert.Empty(t, response.Header().Get("Content-Length"),
		"a handler-set Content-Length must not survive the boundary's own response")
	assert.True(t, strings.HasPrefix(response.Header().Get("Content-Type"), "application/problem+json"),
		"the recovered response must be a Problem Detail, got Content-Type %q", response.Header().Get("Content-Type"))

	entries := logger.snapshot()
	require.Len(t, entries, 1, "exactly one entry must be recorded for the panic")
	entry := entries[0]
	assert.Equal(t, "error", entry.level)
	assert.Equal(t, http.MethodGet, fieldValue(entry, "method"))
	assert.Equal(t, "/boom", fieldValue(entry, "path"))
	assert.Equal(t, "string", fieldValue(entry, "panic_type"))
	assert.Equal(t, false, fieldValue(entry, "response_written"))
	assert.Equal(t, "/boom", fieldValue(entry, "route"))

	serialized := serializedEntry(entry)
	for _, secret := range []string{"secret-token-value", "also-secret", "very-secret", "Authorization", "password"} {
		assert.NotContains(t, serialized, secret, "the panic log must not record %q", secret)
	}
}

// TestPanicBoundaryDoesNotOverwriteCommittedResponse pins the half of the
// contract where a response has already gone out: the boundary must only
// Abort, never attempt a second write, and response_written must read true in
// the log entry.
func TestPanicBoundaryDoesNotOverwriteCommittedResponse(t *testing.T) {
	server, logger := startPanicServer(t, true, plugin.Entry[web.RouteContributor]{
		Identity: plugin.Identity{Plugin: "panictest"},
		Value: fakeRouteContributor{register: func(router *web.Router) {
			router.GET("/partial", func(_ context.Context, c *web.Ctx) error {
				c.String(http.StatusAccepted, "already written")
				panic("boom")
			})
		}},
	})
	engine := testEngineOf(t, server)

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/partial", nil))

	assert.Equal(t, http.StatusAccepted, response.Code)
	assert.Equal(t, "already written", response.Body.String())

	entries := logger.snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, "error", entries[0].level)
	assert.Equal(t, true, fieldValue(entries[0], "response_written"))
}

// TestPanicBoundaryTreatsBrokenConnectionsAsWarnWithoutWriting pins the
// disconnect path: EPIPE/ECONNRESET/http.ErrAbortHandler must log at Warn
// rather than Error, must never attempt to write a response onto a connection
// the client has already walked away from, and must not re-report the
// recovered value as an error for the chain to handle afterwards.
//
// The mapper assertion pins the last of those, and it is what makes this test
// the regression guard for the double-handling bug: recovering inside the
// boundary's own defer makes handle return normally, so the error boundary --
// which sits inside the boundary and would happily map anything it saw --
// cannot observe this panic at all. A boundary that instead returned the
// recovered value (or an error wrapping it) would hand it to the error
// boundary for a second log entry and a second attempt to write onto the dead
// connection, and the mapper would be called.
func TestPanicBoundaryTreatsBrokenConnectionsAsWarnWithoutWriting(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{"EPIPE", syscall.EPIPE},
		{"ECONNRESET", syscall.ECONNRESET},
		{"ErrAbortHandler", http.ErrAbortHandler},
	} {
		t.Run(test.name, func(t *testing.T) {
			mapperCalled := false
			mapper := web.ErrorMapperFunc(func(*web.Ctx, error) (web.ProblemDetail, bool) {
				mapperCalled = true
				return web.ProblemDetail{}, false
			})
			server, logger := startPanicServerWith(t, true, serverInputs{
				routes: []plugin.Entry[web.RouteContributor]{{
					Identity: plugin.Identity{Plugin: "panictest"},
					Value: fakeRouteContributor{register: func(router *web.Router) {
						router.GET("/broken", func(context.Context, *web.Ctx) error {
							panic(test.err)
						})
					}},
				}},
				mappers: []plugin.Entry[web.ErrorMapper]{
					{Identity: plugin.Identity{Plugin: "observer"}, Value: mapper},
				},
			})
			engine := testEngineOf(t, server)

			response := httptest.NewRecorder()
			engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/broken", nil))

			assert.Equal(t, http.StatusOK, response.Code, "aborting must not write a response at all")
			assert.Zero(t, response.Body.Len())
			assert.False(t, mapperCalled, "the boundary must swallow the panic, not re-report it for the error boundary to handle again")

			entries := logger.snapshot()
			require.Len(t, entries, 1)
			assert.Equal(t, "warn", entries[0].level)
		})
	}
}

// TestPanicBoundaryOmitsStackWhenConfiguredOff pins web.recovery.stack: when
// false, no entry carries a "stack" field, on either the Error or the Warn
// path, and the panic value itself is still withheld -- disabling the stack
// must not be a way to get the value into the log instead.
func TestPanicBoundaryOmitsStackWhenConfiguredOff(t *testing.T) {
	server, logger := startPanicServer(t, false, plugin.Entry[web.RouteContributor]{
		Identity: plugin.Identity{Plugin: "panictest"},
		Value: fakeRouteContributor{register: func(router *web.Router) {
			router.GET("/boom", func(context.Context, *web.Ctx) error {
				panic("stack-off-secret")
			})
		}},
	})
	engine := testEngineOf(t, server)

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/boom", nil))

	entries := logger.snapshot()
	require.Len(t, entries, 1)
	assert.False(t, hasField(entries[0], "stack"), "stack must be absent entirely when web.recovery.stack is false")
	assert.NotContains(t, serializedEntry(entries[0]), "stack-off-secret",
		"the panic value must never be recorded, with the stack off least of all")
}

// TestPanicBoundaryIncludesStackWhenConfiguredOn is the companion positive
// case: the default (true) must carry a non-empty stack field.
func TestPanicBoundaryIncludesStackWhenConfiguredOn(t *testing.T) {
	server, logger := startPanicServer(t, true, plugin.Entry[web.RouteContributor]{
		Identity: plugin.Identity{Plugin: "panictest"},
		Value: fakeRouteContributor{register: func(router *web.Router) {
			router.GET("/boom", func(context.Context, *web.Ctx) error {
				panic("boom")
			})
		}},
	})
	engine := testEngineOf(t, server)

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/boom", nil))

	entries := logger.snapshot()
	require.Len(t, entries, 1)
	require.True(t, hasField(entries[0], "stack"))
	stack, ok := fieldValue(entries[0], "stack").(string)
	require.True(t, ok)
	assert.NotEmpty(t, stack)
}

// TestPanicBoundaryWrapsEveryContributedPhaseRecoverMiddleware pins the
// ordering guarantee server.go's pinMiddlewareOutermost(panicBoundaryIdentity)
// is for: a contributed middleware that places itself in PhaseRecover is still
// wrapped by the framework boundary, never the other way around. It proves
// this by panicking from inside the contributed PhaseRecover middleware
// itself -- if the framework boundary did not wrap it, the panic would escape
// to the test's own ServeHTTP call instead of becoming a 500.
func TestPanicBoundaryWrapsEveryContributedPhaseRecoverMiddleware(t *testing.T) {
	contributedRan := false
	contributed := fakeMiddleware{
		order: web.Order{Phase: web.PhaseRecover},
		handler: func(_ context.Context, c *web.Ctx) error {
			contributedRan = true
			panic("contributed phase-recover middleware panicked")
		},
	}

	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	server, ctx, _ := newPingServer(t, cfg, serverInputs{
		middlewares: []plugin.Entry[web.Middleware]{
			{Identity: plugin.Identity{Plugin: "contributed-recover"}, Value: contributed},
		},
	})
	require.NoError(t, server.Start(ctx))
	engine := testEngineOf(t, server)

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ping", nil))

	assert.True(t, contributedRan, "the contributed PhaseRecover middleware must have run")
	assert.Equal(t, http.StatusInternalServerError, response.Code,
		"the framework panic boundary must wrap the contributed PhaseRecover middleware, not sit inside it")
}

func stringifyPanicField(value any) string {
	switch value := value.(type) {
	case string:
		return value
	default:
		return "<value>"
	}
}

// serializedEntry flattens everything an entry recorded -- message and every
// field value, in order -- into one string. A test uses it to assert that a
// secret appears nowhere in what was logged, which a per-field check cannot:
// the value could reach the log through a field the test never thought to
// name, and a flattening check catches that without enumerating them.
func serializedEntry(entry panicLogEntry) string {
	serialized := entry.msg
	for _, value := range entry.fields {
		serialized += " " + stringifyPanicField(value)
	}
	return serialized
}
