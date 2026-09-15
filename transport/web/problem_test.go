package web_test

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

func TestAbortProblemAppliesSafeRFC9457Defaults(t *testing.T) {
	downstreamRan := false
	engine := enginetest.New()
	engine.GET("/orders/{id}", func(_ context.Context, c *web.Ctx) error {
		c.SetHeader("Content-Encoding", "gzip")
		c.SetHeader("Content-Length", "999")
		c.SetHeader("ETag", "secret")
		web.AbortProblem(c, web.NewProblem(http.StatusForbidden, "forbidden"))
		return nil
	}, func(context.Context, *web.Ctx) error {
		downstreamRan = true
		return nil
	})

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/orders/42?token=secret", nil))

	assert.Equal(t, http.StatusForbidden, recorder.Code)
	assert.Equal(t, web.ProblemContentType, recorder.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	assert.Empty(t, recorder.Header().Get("Content-Encoding"))
	assert.Empty(t, recorder.Header().Get("Content-Length"))
	assert.Empty(t, recorder.Header().Get("ETag"))
	// AbortProblem 必须切断后续处理器，这是 IsAborted 在中立面上的可观测形式。
	assert.False(t, downstreamRan, "AbortProblem 之后的处理器不应再运行")

	var problem web.ProblemDetail
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &problem))
	assert.Equal(t, web.ProblemDetail{
		Type:       "about:blank",
		Title:      "Forbidden",
		Status:     http.StatusForbidden,
		Instance:   "/orders/42",
		Properties: map[string]any{"code": "forbidden"},
	}, problem)
	assert.NotContains(t, recorder.Body.String(), "secret")
}

func TestProblemDetailFlattensPropertiesWithoutOverridingStandardMembers(t *testing.T) {
	problem := web.NewProblem(http.StatusUnprocessableEntity, "invalid_order")
	problem.Detail = "Order cannot be processed."
	problem.Properties["trace_id"] = "trace-42"
	problem.Properties["status"] = http.StatusOK
	problem.Properties["title"] = "overridden"

	data, err := json.Marshal(problem)
	require.NoError(t, err)

	var body map[string]any
	require.NoError(t, json.Unmarshal(data, &body))
	assert.Equal(t, float64(http.StatusUnprocessableEntity), body["status"])
	assert.Equal(t, "Unprocessable Entity", body["title"])
	assert.Equal(t, "invalid_order", body["code"])
	assert.Equal(t, "trace-42", body["trace_id"])
	assert.NotContains(t, body, "properties")
}

func TestWriteProblemNormalizesInvalidStatusAndDoesNotOverwriteResponse(t *testing.T) {
	recorder := httptest.NewRecorder()
	c := enginetest.NewCtx(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	web.WriteProblem(c, web.ProblemDetail{Status: 0})
	web.WriteProblem(c, web.NewProblem(http.StatusBadRequest, "late"))

	assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	assert.NotContains(t, recorder.Body.String(), "late")
}

func TestWriteProblemFallsBackWhenAnExtensionCannotBeEncoded(t *testing.T) {
	recorder := httptest.NewRecorder()
	c := enginetest.NewCtx(recorder, httptest.NewRequest(http.MethodGet, "/orders", nil))
	problem := web.NewProblem(http.StatusBadRequest, "bad_extension")
	problem.Properties["not_json"] = math.Inf(1)

	web.WriteProblem(c, problem)

	assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	var response web.ProblemDetail
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, "internal_server_error", response.Properties["code"])
	assert.Equal(t, "/orders", response.Instance)
}
