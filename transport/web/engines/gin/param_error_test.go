package gin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ginlib "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
)

// These tests live in the adapter module because what they exercise is the
// pairing of gin's own binding pipeline with web.ParamError's mapping. The
// neutral test engine has no binding pipeline at all, so the field paths,
// decoder failures, and validator tags asserted below can only be produced
// here. The engine-independent half of ParamError (body overflow, programmer
// errors) stays in transport/web.

type bindingAddress struct {
	City string `json:"city" binding:"required"`
}

type bindingRequest struct {
	Name    string         `json:"name" binding:"required,min=3"`
	Email   string         `json:"email" binding:"required,email"`
	Address bindingAddress `json:"address" binding:"required"`
}

// problemContentType mirrors the unexported constant in transport/web. It is
// spelled out rather than imported because it is deliberately not part of that
// package's public surface.
const problemContentType = "application/problem+json; charset=utf-8"

// run registers chain under method/path on a bare gin engine and serves
// request, driving each handler exactly the way the adapter's own toGinChain
// does -- through web.Handle on the request's single shared Ctx.
func run(method, path string, request *http.Request, chain ...web.Handler) *httptest.ResponseRecorder {
	ginlib.SetMode(ginlib.TestMode)
	engine := ginlib.New()
	converted := make([]ginlib.HandlerFunc, len(chain))
	for i, handler := range chain {
		handle := web.Handle(handler)
		converted[i] = func(c *ginlib.Context) { handle(ctxFor(c)) }
	}
	engine.Handle(method, path, converted...)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	return recorder
}

func serveJSONBindingRequest(body string, destination any) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/requests?secret=query", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	// These tests exercise ParamError's mapping of raw binding failures, so they
	// call gin's binding directly rather than Ctx.Bind, which would wrap the
	// error in ParamError before the test ever sees it.
	return run(http.MethodPost, "/requests", request, func(_ context.Context, c *web.Ctx) error {
		if err := FromCtx(c).ShouldBindJSON(destination); err != nil {
			return web.ParamError(err, destination)
		}
		c.Status(http.StatusNoContent)
		return nil
	})
}

func decodeProblem(t *testing.T, recorder *httptest.ResponseRecorder) web.ProblemDetail {
	t.Helper()
	assert.Equal(t, problemContentType, recorder.Header().Get("Content-Type"))
	var problem web.ProblemDetail
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &problem))
	return problem
}

func decodeFieldErrors(t *testing.T, problem web.ProblemDetail) []web.FieldError {
	t.Helper()
	data, err := json.Marshal(problem.Properties["errors"])
	require.NoError(t, err)
	var fieldErrors []web.FieldError
	require.NoError(t, json.Unmarshal(data, &fieldErrors))
	return fieldErrors
}

func TestParamErrorAdaptsGinJSONBinding(t *testing.T) {
	var destination bindingRequest
	response := serveJSONBindingRequest(
		`{"name":"Alice","email":"alice@example.com","address":{"city":"Shanghai"}}`,
		&destination,
	)

	assert.Equal(t, http.StatusNoContent, response.Code)
	assert.Equal(t, "Alice", destination.Name)
	assert.Equal(t, "Shanghai", destination.Address.City)
}

func TestParamErrorMapsGinJSONDecodeFailures(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "empty body", body: ``},
		{name: "malformed body", body: `{"name":"private-secret"`},
		{name: "wrong field type", body: `{"name":42}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var destination bindingRequest
			response := serveJSONBindingRequest(test.body, &destination)

			assert.Equal(t, http.StatusBadRequest, response.Code)
			problem := decodeProblem(t, response)
			assert.Equal(t, "invalid_request_body", problem.Properties["code"])
			assert.Equal(t, "/requests", problem.Instance)
			assert.NotContains(t, response.Body.String(), "private-secret")
			assert.NotContains(t, response.Body.String(), "query")
		})
	}
}

func TestParamErrorReturnsFieldErrorsWithoutRejectedValues(t *testing.T) {
	var destination bindingRequest
	response := serveJSONBindingRequest(`{"name":"xy","email":"private-secret","address":{}}`, &destination)

	assert.Equal(t, http.StatusBadRequest, response.Code)
	problem := decodeProblem(t, response)
	assert.Equal(t, "validation_failed", problem.Properties["code"])
	assert.Equal(t, []web.FieldError{
		{Field: "address.city", Code: "required", Message: "is required"},
		{Field: "email", Code: "email", Message: "must be a valid email address"},
		{Field: "name", Code: "min", Message: "does not meet the minimum constraint"},
	}, decodeFieldErrors(t, problem))
	assert.NotContains(t, response.Body.String(), "private-secret")
}

func TestParamErrorAdaptsGinQueryBinding(t *testing.T) {
	type queryRequest struct {
		Page int    `form:"page" binding:"gte=1"`
		Sort string `form:"sort" binding:"oneof=name created"`
	}

	serve := func(rawQuery string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/requests?"+rawQuery, nil)
		return run(http.MethodGet, "/requests", request, func(_ context.Context, c *web.Ctx) error {
			var bound queryRequest
			if err := FromCtx(c).ShouldBindQuery(&bound); err != nil {
				return web.ParamError(err, &bound)
			}
			c.Status(http.StatusNoContent)
			return nil
		})
	}

	t.Run("validator errors use form field names", func(t *testing.T) {
		response := serve("page=0&sort=private-secret")

		assert.Equal(t, http.StatusBadRequest, response.Code)
		problem := decodeProblem(t, response)
		assert.Equal(t, "validation_failed", problem.Properties["code"])
		assert.Equal(t, []web.FieldError{
			{Field: "page", Code: "gte", Message: "does not meet the minimum constraint"},
			{Field: "sort", Code: "oneof", Message: "must be one of the allowed values"},
		}, decodeFieldErrors(t, problem))
		assert.NotContains(t, response.Body.String(), "private-secret")
	})

	t.Run("conversion errors are safe invalid requests", func(t *testing.T) {
		response := serve("page=private-secret&sort=name")

		assert.Equal(t, http.StatusBadRequest, response.Code)
		assert.Equal(t, "invalid_request", decodeProblem(t, response).Properties["code"])
		assert.NotContains(t, response.Body.String(), "private-secret")
	})
}

// TestParamErrorRejectsAnInvalidBindingDestination pins the programmer-error
// branch against the engine that can actually reach it: gin hands a nil
// destination straight to encoding/json, which reports InvalidUnmarshalError.
// A caller must never be told its request was malformed when the fault is the
// handler's own.
func TestParamErrorRejectsAnInvalidBindingDestination(t *testing.T) {
	response := serveJSONBindingRequest(`{}`, nil)

	assert.Equal(t, http.StatusInternalServerError, response.Code)
	assert.Equal(t, "internal_server_error", decodeProblem(t, response).Properties["code"])
}

// TestCtxBindAdaptsEngineBindingErrorsLikeParamError pins the end-to-end path
// transport/web cannot reach on its own: Ctx.Bind over an engine that really
// binds. The neutral test engine refuses to bind at all, so only here can a
// successful bind and a validator failure be told apart.
func TestCtxBindAdaptsEngineBindingErrorsLikeParamError(t *testing.T) {
	type payload struct {
		Name string `json:"name" binding:"required"`
	}

	serve := func(body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/requests", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		return run(http.MethodPost, "/requests", request, func(_ context.Context, c *web.Ctx) error {
			var bound payload
			if err := c.Bind(&bound); err != nil {
				return err
			}
			c.Status(http.StatusNoContent)
			return nil
		})
	}

	assert.Equal(t, http.StatusNoContent, serve(`{"name":"Alice"}`).Code)

	bad := serve(`{}`)
	assert.Equal(t, http.StatusBadRequest, bad.Code)
	assert.Equal(t, "validation_failed", decodeProblem(t, bad).Properties["code"])
}

// TestClientIPHonoursTrustedProxies is the discriminating half of the ClientIP
// contract. The untrusted case above does not discriminate: there the engine
// ignores X-Forwarded-For and falls back to RemoteAddr, which is exactly what a
// hand-rolled net.SplitHostPort(RemoteAddr) also returns. Only the trusted case
// separates them -- the engine consults the header it was configured to trust,
// while any self-derived answer keeps reporting the proxy's own address. That
// divergence is not cosmetic: ClientIP keys rate limiting, so an implementation
// that silently ignored trusted-proxy configuration would collapse every client
// behind the proxy into a single bucket.
func TestClientIPHonoursTrustedProxies(t *testing.T) {
	ginlib.SetMode(ginlib.TestMode)
	engine := ginlib.New()
	require.NoError(t, engine.SetTrustedProxies([]string{"127.0.0.1"}))

	var observed string
	handle := web.Handle(func(_ context.Context, c *web.Ctx) error {
		observed = c.ClientIP()
		c.Status(http.StatusNoContent)
		return nil
	})
	engine.GET("/whoami", func(c *ginlib.Context) { handle(ctxFor(c)) })

	call := func(remoteAddr string) string {
		request := httptest.NewRequest(http.MethodGet, "/whoami", nil)
		request.RemoteAddr = remoteAddr
		request.Header.Set("X-Forwarded-For", "9.9.9.9")
		engine.ServeHTTP(httptest.NewRecorder(), request)
		return observed
	}

	assert.Equal(t, "9.9.9.9", call("127.0.0.1:51234"),
		"请求来自受信代理时必须采信 X-Forwarded-For，这是只有真正转发给引擎才能得到的答案")
	assert.Equal(t, "203.0.113.7", call("203.0.113.7:51234"),
		"请求来自非受信代理时必须忽略 X-Forwarded-For，与引擎自身的判断一致")
}
