package web_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

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
