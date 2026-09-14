package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

func errorEngine(mappers ...ErrorMapper) (*gin.Engine, *errorResolver) {
	gin.SetMode(gin.TestMode)
	resolver := newErrorResolver(log.Nop())
	engine := gin.New()
	engine.Use(resolver.attach)
	engine.Use(OnError(mappers...))
	return engine, resolver
}

func performRequest(engine *gin.Engine, method, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	return recorder
}

func TestHandleUsesFirstMapperThatRecognizesWrappedError(t *testing.T) {
	domainErr := errors.New("order version conflict")
	calls := make([]string, 0, 3)
	first := ErrorMapperFunc(func(*Ctx, error) (ProblemDetail, bool) {
		calls = append(calls, "first")
		return ProblemDetail{}, false
	})
	second := ErrorMapperFunc(func(_ *Ctx, err error) (ProblemDetail, bool) {
		calls = append(calls, "second")
		if !errors.Is(err, domainErr) {
			return ProblemDetail{}, false
		}
		problem := NewProblem(http.StatusConflict, "order_version_conflict")
		problem.Detail = "The order was changed by another request."
		return problem, true
	})
	third := ErrorMapperFunc(func(*Ctx, error) (ProblemDetail, bool) {
		calls = append(calls, "third")
		return NewProblem(http.StatusTeapot, "must_not_run"), true
	})
	engine, _ := errorEngine(first, second, third)
	engine.GET("/orders/:id", Handle(func(context.Context, *Ctx) error {
		return fmt.Errorf("update order: %w", domainErr)
	}))

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
	outer := ErrorMapperFunc(func(*Ctx, error) (ProblemDetail, bool) {
		calls = append(calls, "outer")
		return ProblemDetail{}, false
	})
	inner := ErrorMapperFunc(func(_ *Ctx, err error) (ProblemDetail, bool) {
		calls = append(calls, "inner")
		if !errors.Is(err, domainErr) {
			return ProblemDetail{}, false
		}
		return NewProblem(http.StatusConflict, "inventory_conflict"), true
	})
	last := ErrorMapperFunc(func(*Ctx, error) (ProblemDetail, bool) {
		calls = append(calls, "last")
		return NewProblem(http.StatusTeapot, "must_not_run"), true
	})

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(OnError(outer))
	engine.Use(OnError(inner, last))
	engine.GET("/inventory", Handle(func(context.Context, *Ctx) error {
		return fmt.Errorf("reserve inventory: %w", domainErr)
	}))

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
			engine, _ := errorEngine()
			engine.GET("/failure", Handle(func(context.Context, *Ctx) error { return test.err }))

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
		engine, _ := errorEngine()
		nextRan := false
		engine.GET("/failure",
			Handle(func(context.Context, *Ctx) error { return errors.New("failed") }),
			func(c *gin.Context) {
				nextRan = true
				c.Status(http.StatusNoContent)
			},
		)

		response := performRequest(engine, http.MethodGet, "/failure")

		assert.Equal(t, http.StatusInternalServerError, response.Code)
		assert.False(t, nextRan)
	})

	t.Run("committed response is not overwritten", func(t *testing.T) {
		engine, _ := errorEngine()
		engine.GET("/committed", Handle(func(_ context.Context, c *Ctx) error {
			c.String(http.StatusAccepted, "already committed")
			return errors.New("late secret failure")
		}))

		response := performRequest(engine, http.MethodGet, "/committed")

		assert.Equal(t, http.StatusAccepted, response.Code)
		assert.Equal(t, "already committed", response.Body.String())
		assert.NotContains(t, response.Body.String(), "late secret failure")
	})
}

func TestGinReportedErrorsAreJoinedBeforeMapping(t *testing.T) {
	domainErr := errors.New("domain failure")
	mapper := ErrorMapperFunc(func(_ *Ctx, err error) (ProblemDetail, bool) {
		if errors.Is(err, domainErr) {
			return NewProblem(http.StatusUnprocessableEntity, "domain_failure"), true
		}
		return ProblemDetail{}, false
	})
	engine, _ := errorEngine(mapper)
	engine.GET("/reported", func(c *gin.Context) {
		_ = c.Error(fmt.Errorf("first: %w", domainErr))
		_ = c.Error(errors.New("unrelated later failure"))
		c.Abort()
	})

	response := performRequest(engine, http.MethodGet, "/reported")

	assert.Equal(t, http.StatusUnprocessableEntity, response.Code)
	assert.Equal(t, "domain_failure", decodeProblem(t, response).Properties["code"])
}

func TestMapperCannotTurnAnErrorIntoNonErrorHTTPStatus(t *testing.T) {
	for _, status := range []int{0, http.StatusContinue, http.StatusOK, http.StatusFound, 399, 600} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			mapper := ErrorMapperFunc(func(*Ctx, error) (ProblemDetail, bool) {
				problem := ProblemDetail{
					Status:     status,
					Detail:     "mapper secret",
					Properties: map[string]any{"code": "false_success", "secret": "private"},
				}
				return problem, true
			})
			engine, _ := errorEngine(mapper)
			engine.GET("/failure", Handle(func(context.Context, *Ctx) error { return errors.New("failed") }))

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

func (p *mappedRoutePlugin) RegisterRoutes(r *Router) {
	r.GET("/mapped-error", Handle(func(context.Context, *Ctx) error {
		return fmt.Errorf("service failed: %w", p.domainErr)
	})).Name("mapped.error")
}

func (p *mappedRoutePlugin) MapError(_ *Ctx, err error) (ProblemDetail, bool) {
	if !errors.Is(err, p.domainErr) {
		return ProblemDetail{}, false
	}
	return NewProblem(http.StatusServiceUnavailable, "dependency_unavailable"), true
}

func (*mappedRoutePlugin) ErrorOrder() ErrorOrder { return ErrorOrder{} }

func TestServerUsesInjectedErrorMapperAndObservationSeesMappedStatus(t *testing.T) {
	domainErr := errors.New("database unavailable")
	application := &mappedRoutePlugin{domainErr: domainErr}
	observedStatus := 0
	observer := fakeMiddleware{
		order: Order{Phase: PhaseObserve},
		handler: func(c *gin.Context) {
			c.Next()
			observedStatus = c.Writer.Status()
		},
	}
	cfg := DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	server, ctx, _ := newPingServer(t, cfg, serverInputs{
		middlewares: []plugin.Entry[Middleware]{
			{Identity: plugin.Identity{Plugin: "observer"}, Value: observer},
		},
		routes: []plugin.Entry[RouteContributor]{
			{Identity: plugin.Identity{Plugin: "orders"}, Value: application},
		},
		mappers: []ErrorMapper{application},
	})
	require.NoError(t, server.Start(ctx))

	response := performRequest(server.engine, http.MethodGet, "/mapped-error")

	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	assert.Equal(t, "dependency_unavailable", decodeProblem(t, response).Properties["code"])
	assert.Equal(t, http.StatusServiceUnavailable, observedStatus)
}

func TestErrorMapperPublicAdaptersHaveExpectedShape(t *testing.T) {
	var mapper ErrorMapper = ErrorMapperFunc(func(*Ctx, error) (ProblemDetail, bool) {
		return ProblemDetail{}, false
	})
	assert.NotNil(t, mapper)
	assert.PanicsWithValue(t, "xbc: web.Handle requires a non-nil handler", func() { Handle(nil) })
}
