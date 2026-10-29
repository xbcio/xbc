package web_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

// errorEngine builds the two-handler front of a request the error boundary
// needs: a resolver published on the request, then OnError scoping the mappers
// under test. Callers register their own route afterwards.
func errorEngine(mappers ...web.ErrorMapper) *enginetest.Engine {
	resolver := web.NewErrorResolver(log.Nop())
	engine := enginetest.New()
	engine.Use(func(_ context.Context, c *web.Ctx) error {
		resolver.Attach(c)
		return nil
	})
	engine.Use(web.OnError(mappers...))
	return engine
}

func performRequest(engine *enginetest.Engine, method, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	return recorder
}

func TestHandleUsesFirstMapperThatRecognizesWrappedError(t *testing.T) {
	domainErr := errors.New("order version conflict")
	calls := make([]string, 0, 3)
	first := web.ErrorMapperFunc(func(*web.Ctx, error) (web.ProblemDetail, bool) {
		calls = append(calls, "first")
		return web.ProblemDetail{}, false
	})
	second := web.ErrorMapperFunc(func(_ *web.Ctx, err error) (web.ProblemDetail, bool) {
		calls = append(calls, "second")
		if !errors.Is(err, domainErr) {
			return web.ProblemDetail{}, false
		}
		problem := web.NewProblem(http.StatusConflict, "order_version_conflict")
		problem.Detail = "The order was changed by another request."
		return problem, true
	})
	third := web.ErrorMapperFunc(func(*web.Ctx, error) (web.ProblemDetail, bool) {
		calls = append(calls, "third")
		return web.NewProblem(http.StatusTeapot, "must_not_run"), true
	})
	engine := errorEngine(first, second, third)
	engine.GET("/orders/{id}", func(context.Context, *web.Ctx) error {
		return fmt.Errorf("update order: %w", domainErr)
	})

	response := performRequest(engine, http.MethodGet, "/orders/42?token=secret")

	assert.Equal(t, http.StatusConflict, response.Code)
	assert.Equal(t, []string{"first", "second"}, calls)
	problem := decodeProblem(t, response)
	assert.Equal(t, "order_version_conflict", problem.Properties["code"])
	assert.Equal(t, "The order was changed by another request.", problem.Detail)
	assert.Equal(t, "/orders/42", problem.Instance)
	assert.NotContains(t, response.Body.String(), "token")
}

func TestNestedOnErrorComposesMappersOuterToInner(t *testing.T) {
	domainErr := errors.New("inventory conflict")
	calls := make([]string, 0, 3)
	outer := web.ErrorMapperFunc(func(*web.Ctx, error) (web.ProblemDetail, bool) {
		calls = append(calls, "outer")
		return web.ProblemDetail{}, false
	})
	inner := web.ErrorMapperFunc(func(_ *web.Ctx, err error) (web.ProblemDetail, bool) {
		calls = append(calls, "inner")
		if !errors.Is(err, domainErr) {
			return web.ProblemDetail{}, false
		}
		return web.NewProblem(http.StatusConflict, "inventory_conflict"), true
	})
	last := web.ErrorMapperFunc(func(*web.Ctx, error) (web.ProblemDetail, bool) {
		calls = append(calls, "last")
		return web.NewProblem(http.StatusTeapot, "must_not_run"), true
	})

	engine := enginetest.New()
	engine.Use(web.OnError(outer))
	engine.Use(web.OnError(inner, last))
	engine.GET("/inventory", func(context.Context, *web.Ctx) error {
		return fmt.Errorf("reserve inventory: %w", domainErr)
	})

	response := performRequest(engine, http.MethodGet, "/inventory")

	assert.Equal(t, http.StatusConflict, response.Code)
	assert.Equal(t, []string{"outer", "inner"}, calls)
	assert.Equal(t, "inventory_conflict", decodeProblem(t, response).Properties["code"])
}

func TestUnknownAndDeadlineErrorsUseSafeDefaults(t *testing.T) {
	secret := "postgres://admin:private-password@database/orders"
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "unknown", err: errors.New(secret), status: http.StatusInternalServerError, code: "internal_server_error"},
		{name: "wrapped deadline", err: fmt.Errorf("repository: %w", context.DeadlineExceeded), status: http.StatusGatewayTimeout, code: "gateway_timeout"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine := errorEngine()
			engine.GET("/failure", func(context.Context, *web.Ctx) error { return test.err })

			response := performRequest(engine, http.MethodGet, "/failure")

			assert.Equal(t, test.status, response.Code)
			problem := decodeProblem(t, response)
			assert.Equal(t, test.code, problem.Properties["code"])
			assert.NotContains(t, response.Body.String(), secret)
			assert.Empty(t, problem.Detail)
		})
	}
}

// lookalikeError carries the text of a standard-library sentinel without being
// that sentinel. A client controls what a rejected value says, so a mapping that
// matched on message text could be steered by the request itself.
type lookalikeError string

func (e lookalikeError) Error() string { return string(e) }

// TestReturnedBodyReadFailuresKeepTheStatusParamErrorWouldChoose pins the
// fallback for a handler that returns a body-read failure as-is instead of
// passing it through ParamError. Both overflow shapes reach 413: the body
// reader's own *http.MaxBytesError, and multipart's fixed limits, which fire
// before that reader's cap and are a different type.
//
// The look-alike rows are the other half. They carry the same text without the
// identity, and must stay 500: matching on text would let a request choose its
// own status.
func TestReturnedBodyReadFailuresKeepTheStatusParamErrorWouldChoose(t *testing.T) {
	secret := "postgres://admin:private-password@database/orders"
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "max bytes error", err: &http.MaxBytesError{Limit: 8}, status: http.StatusRequestEntityTooLarge, code: "request_body_too_large"},
		{name: "wrapped max bytes error", err: fmt.Errorf("read upload: %w", &http.MaxBytesError{Limit: 8}), status: http.StatusRequestEntityTooLarge, code: "request_body_too_large"},
		{name: "multipart message too large", err: multipart.ErrMessageTooLarge, status: http.StatusRequestEntityTooLarge, code: "request_body_too_large"},
		{name: "wrapped multipart message too large", err: fmt.Errorf("x: %w", multipart.ErrMessageTooLarge), status: http.StatusRequestEntityTooLarge, code: "request_body_too_large"},
		{name: "multipart text without the sentinel", err: lookalikeError("multipart: message too large"), status: http.StatusInternalServerError, code: "internal_server_error"},
		{name: "multipart sentinel flattened to text", err: fmt.Errorf("x: %s", multipart.ErrMessageTooLarge), status: http.StatusInternalServerError, code: "internal_server_error"},
		{name: "max bytes text without the type", err: lookalikeError("http: request body too large"), status: http.StatusInternalServerError, code: "internal_server_error"},
		{name: "unrelated error mentioning the secret", err: errors.New(secret), status: http.StatusInternalServerError, code: "internal_server_error"},
		// Ctx.Bind and ParamError already own this error: they classify it as a
		// bad request, and that classification travels in a requestError the
		// fallback must not look past.
		{name: "multipart sentinel adapted by ParamError stays a bad request", err: web.ParamError(multipart.ErrMessageTooLarge, nil), status: http.StatusBadRequest, code: "invalid_request"},
		{name: "max bytes error adapted by ParamError keeps its 413", err: web.ParamError(&http.MaxBytesError{Limit: 8}, nil), status: http.StatusRequestEntityTooLarge, code: "request_body_too_large"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine := errorEngine()
			engine.GET("/failure", func(context.Context, *web.Ctx) error { return test.err })

			response := performRequest(engine, http.MethodGet, "/failure")

			assert.Equal(t, test.status, response.Code)
			problem := decodeProblem(t, response)
			assert.Equal(t, test.code, problem.Properties["code"])
			assert.NotContains(t, response.Body.String(), secret)
			assert.NotContains(t, response.Body.String(), "message too large")
		})
	}
}

// TestReadDeadlineBecomes408OnlyWhenTheRequestContextIsCancelled pins the
// condition that separates a client that stalled from an upstream that did.
//
// An expired connection read deadline and an expired upstream call deadline are
// the same error value, os.ErrDeadlineExceeded. What differs is the request:
// net/http cancels its context when a read fails, and leaves it live when the
// handler's own outbound call timed out. Only the first is the client's doing,
// so only the first is a 408; the second keeps the 500 it always had.
func TestReadDeadlineBecomes408OnlyWhenTheRequestContextIsCancelled(t *testing.T) {
	readTimeout := &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	tests := []struct {
		name      string
		err       error
		cancelled bool
		status    int
		code      string
	}{
		{name: "connection read deadline with a cancelled request", err: fmt.Errorf("read body: %w", readTimeout), cancelled: true, status: http.StatusRequestTimeout, code: "request_timeout"},
		{name: "bare deadline sentinel with a cancelled request", err: os.ErrDeadlineExceeded, cancelled: true, status: http.StatusRequestTimeout, code: "request_timeout"},
		{name: "deadline error with a live request", err: os.ErrDeadlineExceeded, status: http.StatusInternalServerError, code: "internal_server_error"},
		{name: "wrapped connection deadline with a live request", err: fmt.Errorf("call upstream: %w", readTimeout), status: http.StatusInternalServerError, code: "internal_server_error"},
		{name: "cancelled request with an unrelated error", err: errors.New("client went away"), cancelled: true, status: http.StatusInternalServerError, code: "internal_server_error"},
		{name: "context deadline stays 504 on a cancelled request", err: context.DeadlineExceeded, cancelled: true, status: http.StatusGatewayTimeout, code: "gateway_timeout"},
		{name: "an error that is both stays 504", err: errors.Join(readTimeout, context.DeadlineExceeded), cancelled: true, status: http.StatusGatewayTimeout, code: "gateway_timeout"},
		{name: "bind path keeps its 400", err: web.ParamError(readTimeout, nil), cancelled: true, status: http.StatusBadRequest, code: "invalid_request"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine := errorEngine()
			engine.GET("/failure", func(context.Context, *web.Ctx) error { return test.err })
			ctx, cancel := context.WithCancel(context.Background())
			if test.cancelled {
				cancel()
			}
			defer cancel()

			response := httptest.NewRecorder()
			engine.ServeHTTP(response, httptest.NewRequestWithContext(ctx, http.MethodGet, "/failure", nil))

			assert.Equal(t, test.status, response.Code)
			problem := decodeProblem(t, response)
			assert.Equal(t, test.code, problem.Properties["code"])
		})
	}
}

// TestConfiguredMapperPrecedesTheBuiltInReadErrorMappings keeps the order the
// error boundary documents: an application mapper that claims a body-read
// failure decides its status, and the built-in 413 and 408 only apply to what
// no mapper claimed.
func TestConfiguredMapperPrecedesTheBuiltInReadErrorMappings(t *testing.T) {
	mapper := web.ErrorMapperFunc(func(_ *web.Ctx, err error) (web.ProblemDetail, bool) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) || errors.Is(err, os.ErrDeadlineExceeded) {
			return web.NewProblem(http.StatusUnprocessableEntity, "app_decided"), true
		}
		return web.ProblemDetail{}, false
	})
	for name, err := range map[string]error{
		"max bytes error":   &http.MaxBytesError{Limit: 8},
		"deadline exceeded": os.ErrDeadlineExceeded,
	} {
		t.Run(name, func(t *testing.T) {
			engine := errorEngine(mapper)
			engine.GET("/failure", func(context.Context, *web.Ctx) error { return err })
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			response := httptest.NewRecorder()
			engine.ServeHTTP(response, httptest.NewRequestWithContext(ctx, http.MethodGet, "/failure", nil))

			assert.Equal(t, http.StatusUnprocessableEntity, response.Code)
			assert.Equal(t, "app_decided", decodeProblem(t, response).Properties["code"])
		})
	}
}

// readTestRoutes registers the two routes the connection-level read-deadline
// tests drive. slow-read is the client-stall case: the handler shortens its own
// read deadline, reads the body, and returns the read error untouched, which is
// what a handler that does not route it through ParamError does. upstream is
// the control: an error with the same identity but no stalled client behind it.
func readTestRoutes() plugin.Entry[web.RouteContributor] {
	return plugin.Entry[web.RouteContributor]{
		Identity: plugin.Identity{Plugin: "readtest"},
		Value: fakeRouteContributor{register: func(router *web.Router) {
			router.POST("/slow-read", func(_ context.Context, c *web.Ctx) error {
				controller := http.NewResponseController(c.Writer())
				if err := controller.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
					return fmt.Errorf("the writer chain does not reach the connection: %w", err)
				}
				_, err := io.ReadAll(c.Request().Body)
				return err
			})
			router.GET("/upstream", func(context.Context, *web.Ctx) error {
				return fmt.Errorf("call upstream: %w", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded})
			})
		}},
	}
}

func decodeProblemResponse(t *testing.T, response *http.Response) web.ProblemDetail {
	t.Helper()
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	var problem web.ProblemDetail
	require.NoError(t, json.Unmarshal(body, &problem), "body: %s", body)
	return problem
}

// TestStalledBodyReadIs408OverARealConnection drives the whole path with a real
// socket: a client announces 100 body bytes, sends 3, and goes quiet; the
// handler's own SetReadDeadline expires mid-read. Only a real connection
// produces the error chain this mapping exists for -- *net.OpError wrapping the
// poll package's deadline error, with the request context cancelled by net/http
// -- so a hand-built error would not prove the condition holds on the wire.
func TestStalledBodyReadIs408OverARealConnection(t *testing.T) {
	_, addr := servingPingServer(t, serverInputs{routes: []plugin.Entry[web.RouteContributor]{readTestRoutes()}})

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = io.WriteString(conn, "POST /slow-read HTTP/1.1\r\nHost: example.test\r\nContent-Type: text/plain\r\nContent-Length: 100\r\n\r\nabc")
	require.NoError(t, err)

	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err, "the server must answer instead of leaving the stalled client hanging")

	assert.Equal(t, http.StatusRequestTimeout, response.StatusCode)
	problem := decodeProblemResponse(t, response)
	assert.Equal(t, "request_timeout", problem.Properties["code"])
	assert.Empty(t, problem.Detail)
}

// TestUpstreamDeadlineErrorOnALiveRequestStaysAServerError is the counterpart
// that keeps the 408 honest: the handler's own outbound call timed out, the
// client is still connected and its request context is alive, so blaming the
// client with a 408 would be wrong. The status is the one this error always
// had.
func TestUpstreamDeadlineErrorOnALiveRequestStaysAServerError(t *testing.T) {
	_, addr := servingPingServer(t, serverInputs{routes: []plugin.Entry[web.RouteContributor]{readTestRoutes()}})

	response, err := http.Get("http://" + addr + "/upstream")
	require.NoError(t, err)

	assert.Equal(t, http.StatusInternalServerError, response.StatusCode)
	assert.Equal(t, "internal_server_error", decodeProblemResponse(t, response).Properties["code"])
}

func TestHandleAbortsRemainingHandlersAndPreservesCommittedResponse(t *testing.T) {
	t.Run("returned error aborts chain", func(t *testing.T) {
		engine := errorEngine()
		nextRan := false
		engine.GET("/failure",
			func(context.Context, *web.Ctx) error { return errors.New("failed") },
			func(_ context.Context, c *web.Ctx) error {
				nextRan = true
				c.Status(http.StatusNoContent)
				return nil
			},
		)

		response := performRequest(engine, http.MethodGet, "/failure")

		assert.Equal(t, http.StatusInternalServerError, response.Code)
		assert.False(t, nextRan)
	})

	t.Run("committed response is not overwritten", func(t *testing.T) {
		engine := errorEngine()
		engine.GET("/committed", func(_ context.Context, c *web.Ctx) error {
			c.String(http.StatusAccepted, "already committed")
			return errors.New("late secret failure")
		})

		response := performRequest(engine, http.MethodGet, "/committed")

		assert.Equal(t, http.StatusAccepted, response.Code)
		assert.Equal(t, "already committed", response.Body.String())
		assert.NotContains(t, response.Body.String(), "late secret failure")
	})
}

func TestMapperCannotTurnAnErrorIntoNonErrorHTTPStatus(t *testing.T) {
	for _, status := range []int{0, http.StatusContinue, http.StatusOK, http.StatusFound, 399, 600} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			mapper := web.ErrorMapperFunc(func(*web.Ctx, error) (web.ProblemDetail, bool) {
				problem := web.ProblemDetail{
					Status:     status,
					Detail:     "mapper secret",
					Properties: map[string]any{"code": "false_success", "secret": "private"},
				}
				return problem, true
			})
			engine := errorEngine(mapper)
			engine.GET("/failure", func(context.Context, *web.Ctx) error { return errors.New("failed") })

			response := performRequest(engine, http.MethodGet, "/failure")

			assert.Equal(t, http.StatusInternalServerError, response.Code)
			problem := decodeProblem(t, response)
			assert.Equal(t, "internal_server_error", problem.Properties["code"])
			assert.Empty(t, problem.Detail)
			assert.NotContains(t, response.Body.String(), "mapper secret")
			assert.NotContains(t, response.Body.String(), "private")
		})
	}
}

type mappedRoutePlugin struct {
	domainErr error
}

func (p *mappedRoutePlugin) RegisterRoutes(r *web.Router) {
	r.GET("/mapped-error", func(context.Context, *web.Ctx) error {
		return fmt.Errorf("service failed: %w", p.domainErr)
	}).Name("mapped.error")
}

func (p *mappedRoutePlugin) MapError(_ *web.Ctx, err error) (web.ProblemDetail, bool) {
	if !errors.Is(err, p.domainErr) {
		return web.ProblemDetail{}, false
	}
	return web.NewProblem(http.StatusServiceUnavailable, "dependency_unavailable"), true
}

func (*mappedRoutePlugin) ErrorOrder() web.ErrorOrder { return web.ErrorOrder{} }

func TestServerUsesInjectedErrorMapperAndObservationSeesMappedStatus(t *testing.T) {
	domainErr := errors.New("database unavailable")
	application := &mappedRoutePlugin{domainErr: domainErr}
	observedStatus := 0
	observer := fakeMiddleware{
		order: web.Order{Phase: web.PhaseObserve},
		handler: func(_ context.Context, c *web.Ctx) error {
			c.Next()
			observedStatus = c.Writer().Status()
			return nil
		},
	}
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	server, ctx, _ := newPingServer(t, cfg, serverInputs{
		middlewares: []plugin.Entry[web.Middleware]{
			{Identity: plugin.Identity{Plugin: "observer"}, Value: observer},
		},
		routes: []plugin.Entry[web.RouteContributor]{
			{Identity: plugin.Identity{Plugin: "orders"}, Value: application},
		},
		mappers: []plugin.Entry[web.ErrorMapper]{
			{Identity: plugin.Identity{Plugin: "orders"}, Value: application},
		},
	})
	require.NoError(t, server.Start(ctx))

	response := performRequest(testEngineOf(t, server), http.MethodGet, "/mapped-error")

	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	assert.Equal(t, "dependency_unavailable", decodeProblem(t, response).Properties["code"])
	assert.Equal(t, http.StatusServiceUnavailable, observedStatus)
}

func TestErrorMapperPublicAdaptersHaveExpectedShape(t *testing.T) {
	var mapper web.ErrorMapper = web.ErrorMapperFunc(func(*web.Ctx, error) (web.ProblemDetail, bool) {
		return web.ProblemDetail{}, false
	})
	assert.NotNil(t, mapper)
	assert.PanicsWithValue(t, "xbc: web.Handle requires a non-nil handler", func() { web.Handle(nil) })
}

// stubLogger captures the fields of each Error entry so a test can tell a
// failure that was recorded from one that was merely absorbed.
type stubLogger struct {
	log.Logger
	entries [][]any
}

func (l *stubLogger) Error(_ string, kv ...any) {
	l.entries = append(l.entries, kv)
}

// TestErrorAfterCommitIsLoggedAndNeverReachesTheClient pins the half of the
// error boundary that has no response left to write: the failure must still be
// recorded in full, and the response the client is already receiving must stay
// exactly as it is.
//
// The mapper claims the error as 4xx on purpose. That is what makes this test
// discriminating: the resolver's other logging branch only fires at 5xx, so a
// regression that drops the already-committed branch would leave this failure
// with no record anywhere while every other test stayed green. An engine
// adapter draining a native middleware's error accumulator after the response
// went out reaches exactly this path.
func TestErrorAfterCommitIsLoggedAndNeverReachesTheClient(t *testing.T) {
	logger := &stubLogger{Logger: log.Nop()}
	internal := errors.New("connection string rejected by host db-7")
	mapper := web.ErrorMapperFunc(func(*web.Ctx, error) (web.ProblemDetail, bool) {
		return web.NewProblem(http.StatusConflict, "conflict"), true
	})

	resolver := web.NewErrorResolver(logger)
	engine := enginetest.New()
	engine.Use(func(_ context.Context, c *web.Ctx) error {
		resolver.Attach(c)
		return nil
	})
	engine.Use(web.OnError(mapper))
	engine.GET("/committed", func(_ context.Context, c *web.Ctx) error {
		c.String(http.StatusOK, "response already sent")
		return internal
	})

	response := performRequest(engine, http.MethodGet, "/committed")

	assert.Equal(t, http.StatusOK, response.Result().StatusCode, "an already committed response must not be rewritten")
	assert.Equal(t, "response already sent", response.Body.String(), "Internal error detail must not leak to the caller")
	require.Len(t, logger.entries, 1, "an error raised after the response was committed must still be logged, not disappear silently")
	assert.Contains(t, fmt.Sprint(logger.entries[0]...), internal.Error(), "the log must record the failure in full detail")
}
