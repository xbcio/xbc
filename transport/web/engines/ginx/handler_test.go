package ginx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ginlib "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

// TestHandlerReadsTheNativeGinContext covers the three things a gin-native
// terminal handler does in practice -- read route parameters and the query
// string, bind a JSON body, and render a JSON response. Each is answered by the
// engine itself rather than by a reconstruction: a context built with
// CreateTestContextOnly has no route table to read a :param from, so the first
// case would silently see an empty parameter there.
func TestHandlerReadsTheNativeGinContext(t *testing.T) {
	type probe struct {
		param string
		query string
		name  string
	}
	cases := []struct {
		name     string
		method   string
		pattern  string
		target   string
		body     string
		handler  func(*probe) ginlib.HandlerFunc
		want     probe
		wantCode int
		wantJSON string
	}{
		{
			name:    "route parameters and query string",
			method:  http.MethodGet,
			pattern: "/items/:id",
			target:  "/items/42?q=xbc",
			handler: func(p *probe) ginlib.HandlerFunc {
				return func(c *ginlib.Context) {
					p.param = c.Param("id")
					p.query = c.Query("q")
					c.Status(http.StatusNoContent)
				}
			},
			want:     probe{param: "42", query: "xbc"},
			wantCode: http.StatusNoContent,
		},
		{
			name:    "JSON response",
			method:  http.MethodGet,
			pattern: "/json",
			target:  "/json",
			handler: func(*probe) ginlib.HandlerFunc {
				return func(c *ginlib.Context) {
					c.JSON(http.StatusCreated, ginlib.H{"name": "xbc"})
				}
			},
			wantCode: http.StatusCreated,
			wantJSON: `{"name":"xbc"}`,
		},
		{
			name:    "JSON request body",
			method:  http.MethodPost,
			pattern: "/echo",
			target:  "/echo",
			body:    `{"name":"bound"}`,
			handler: func(p *probe) ginlib.HandlerFunc {
				return func(c *ginlib.Context) {
					var payload struct {
						Name string `json:"name"`
					}
					if err := c.ShouldBindJSON(&payload); err != nil {
						//nolint:errcheck // Error returns the entry it recorded; only the side effect matters here.
						c.Error(err)
						return
					}
					p.name = payload.Name
					c.JSON(http.StatusOK, payload)
				}
			},
			want:     probe{name: "bound"},
			wantCode: http.StatusOK,
			wantJSON: `{"name":"bound"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			port, engine := newTestEngine(t)
			got := &probe{}
			port.Handle(tc.method, tc.pattern, []web.Handler{Handler(tc.handler(got))})

			request := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			if tc.body != "" {
				request.Header.Set("Content-Type", "application/json")
			}
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)

			assert.Equal(t, tc.wantCode, response.Code)
			assert.Equal(t, tc.want, *got)
			if tc.wantJSON != "" {
				assert.JSONEq(t, tc.wantJSON, response.Body.String())
			}
		})
	}
}

// TestHandlerReportsErrorsRecordedDuringItsOwnCall pins the seam between gin's
// error accumulator and xbc's error boundary on the terminal-handler side. A
// gin-native handler reports failure by pushing onto Context.Errors instead of
// returning, and Handler is the only door such a handler enters an xbc chain
// through, so draining it here is what keeps the failure from vanishing.
func TestHandlerReportsErrorsRecordedDuringItsOwnCall(t *testing.T) {
	port, engine := newTestEngine(t)

	recorded := errors.New("recorded by the native handler")
	var reported error
	// The boundary that would normally render this error is transport/web's;
	// capturing it directly keeps this test about the drain and nothing else.
	port.Handle(http.MethodGet, "/captured", []web.Handler{
		func(_ context.Context, c *web.Ctx) error {
			reported = Handler(func(gc *ginlib.Context) {
				//nolint:errcheck // Error returns the entry it recorded; only the side effect matters here.
				gc.Error(recorded)
			})(context.Background(), c)
			return nil
		},
	})

	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/captured", nil))

	assert.ErrorIs(t, reported, recorded, "an error the handler recorded during its own call must be reported out")
}

// TestHandlerReportedErrorsAreRenderedThroughTheErrorBoundary is the end-to-end
// half of the same seam, mirroring Wrap's equivalent test. The test above proves
// only that the Handler returns the error; the whole path from gin's accumulator
// through the error boundary to a rendered Problem Detail could break with it
// still green, so the judgement here is the response, never the returned error.
//
// Two errors are pushed on purpose, and only the first wraps the domain error
// the mapper recognizes. Joining them is what keeps errors.Is reachable through
// both: an implementation reporting only the last entry would miss the mapper
// and fall through to the generic 500.
func TestHandlerReportedErrorsAreRenderedThroughTheErrorBoundary(t *testing.T) {
	port, engine := newTestEngine(t)

	domainErr := errors.New("domain failure")
	mapper := web.ErrorMapperFunc(func(_ *web.Ctx, err error) (web.ProblemDetail, bool) {
		if errors.Is(err, domainErr) {
			return web.NewProblem(http.StatusUnprocessableEntity, "domain_failure"), true
		}
		return web.ProblemDetail{}, false
	})
	port.Handle(http.MethodGet, "/reported", []web.Handler{
		web.OnError(mapper),
		Handler(func(gc *ginlib.Context) {
			//nolint:errcheck // Error returns the entry it recorded; only the side effect matters here.
			gc.Error(fmt.Errorf("first: %w", domainErr))
			//nolint:errcheck // Same: the unrelated second entry is pushed for its side effect.
			gc.Error(errors.New("unrelated later failure"))
			gc.Abort()
		}),
	})

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/reported", nil))

	require.Equal(t, http.StatusUnprocessableEntity, response.Code,
		"an error reported by a gin-native handler must be mapped through the error boundary and rendered")
	assert.Equal(t, "domain_failure", decodeProblem(t, response).Properties["code"],
		"the join must keep errors.Is reachable through every recorded entry -- otherwise an implementation that reports only the last error would pass too")
}

// TestHandlerReportsErrorsEvenAfterTheResponseIsCommitted covers the half of
// the seam the two tests above leave open, at parity with Wrap: a handler that
// has already sent a response and only then records a failure must not have
// that failure discarded. transport/web's boundary logs a failure it can no
// longer render and leaves the committed response untouched, so the internal
// detail stays in the log instead of following the error out to the caller.
func TestHandlerReportsErrorsEvenAfterTheResponseIsCommitted(t *testing.T) {
	port, engine := newTestEngine(t)

	recorded := errors.New("internal detail the caller must never see")
	// A mapper that claims every error makes the "response left alone" half
	// real: without one, an implementation that rewrote the committed response
	// would still answer 200 and look correct here.
	mapper := web.ErrorMapperFunc(func(*web.Ctx, error) (web.ProblemDetail, bool) {
		return web.NewProblem(http.StatusInternalServerError, "mapped_after_commit"), true
	})

	var reported error
	port.Handle(http.MethodGet, "/committed", []web.Handler{
		web.OnError(mapper),
		func(ctx context.Context, c *web.Ctx) error {
			reported = Handler(func(gc *ginlib.Context) {
				gc.String(http.StatusOK, "already sent")
				//nolint:errcheck // Error returns the entry it recorded; only the side effect matters here.
				gc.Error(recorded)
			})(ctx, c)
			// Returning it is what a Handler-registered route does on its own;
			// capturing it as well keeps the drain itself observable.
			return reported
		},
	})

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/committed", nil))

	assert.ErrorIs(t, reported, recorded, "a committed response is no reason to discard the error -- it must still be reported to the error boundary")
	assert.Equal(t, http.StatusOK, response.Code, "a committed response must not be rewritten")
	assert.Equal(t, "already sent", response.Body.String())
	assert.NotContains(t, response.Body.String(), recorded.Error(), "the internal error detail belongs in the log only and must not be returned to the caller")
}

// TestHandlerIgnoresErrorsRecordedBeforeItRan pins the before/after delta.
// Context.Errors is a shared per-request slice, so reporting everything in it
// would attribute an earlier middleware's failure to this handler and render it
// a second time.
func TestHandlerIgnoresErrorsRecordedBeforeItRan(t *testing.T) {
	port, engine := newTestEngine(t)

	var reported error
	port.Handle(http.MethodGet, "/earlier", []web.Handler{
		func(_ context.Context, c *web.Ctx) error {
			//nolint:errcheck // Only the recorded entry matters here.
			FromCtx(c).Error(errors.New("recorded by an earlier middleware"))
			reported = Handler(func(*ginlib.Context) {})(context.Background(), c)
			return nil
		},
	})

	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/earlier", nil))

	assert.NoError(t, reported, "an error recorded earlier in the chain does not belong to this handler and must not be reported by it")
}

// TestHandlerRunsWhereverTheEngineRoutesIt pins that FromCtx really does yield
// the native context on the two registration paths besides a plain route: a
// mounted subtree, and a chain where a gin-native middleware adapted by Wrap
// advances the chain into the terminal Handler through c.Next.
func TestHandlerRunsWhereverTheEngineRoutesIt(t *testing.T) {
	t.Run("under a mounted subtree", func(t *testing.T) {
		port, engine := newTestEngine(t)

		var ran bool
		port.Mount(http.MethodGet, "/mounted", []web.Handler{
			Handler(func(gc *ginlib.Context) {
				ran = true
				gc.Status(http.StatusNoContent)
			}),
		})

		response := httptest.NewRecorder()
		engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/mounted/deep", nil))

		assert.Equal(t, http.StatusNoContent, response.Code)
		assert.True(t, ran, "a mounted chain must reach the native handler")
	})

	t.Run("after a Wrap middleware advanced the chain", func(t *testing.T) {
		port, engine := newTestEngine(t)

		var order []string
		port.Handle(http.MethodGet, "/after/:id", []web.Handler{
			Wrap(ginlib.HandlerFunc(func(gc *ginlib.Context) {
				order = append(order, "middleware")
				gc.Next()
			}), web.Order{}).Handler(),
			Handler(func(gc *ginlib.Context) {
				order = append(order, "handler:"+gc.Param("id"))
				gc.Status(http.StatusNoContent)
			}),
		})

		response := httptest.NewRecorder()
		engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/after/7", nil))

		assert.Equal(t, http.StatusNoContent, response.Code)
		assert.Equal(t, []string{"middleware", "handler:7"}, order,
			"the terminal handler must run after the wrapped middleware and still see the routed parameter")
	})
}

// TestHandlerRefusesToRunOnAnotherEngine pins the same contract as Wrap: a
// gin-native handler that quietly does not run is a route that has disappeared
// without a trace, so the adapter refuses loudly instead of skipping.
func TestHandlerRefusesToRunOnAnotherEngine(t *testing.T) {
	var ran bool
	c := enginetest.NewCtx(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	err := Handler(func(*ginlib.Context) { ran = true })(context.Background(), c)

	require.Error(t, err, "a request gin is not serving must raise an error rather than be skipped silently")
	assert.ErrorIs(t, err, errNotGinEngine)
	assert.False(t, ran, "a gin-native handler must not run on another engine")
}

func TestHandlerPanicsOnNilHandler(t *testing.T) {
	assert.PanicsWithValue(t, "xbc: gin.Handler requires a non-nil handler", func() {
		Handler(nil)
	})
}
