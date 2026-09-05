package web

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAbortProblemAppliesSafeRFC9457Defaults(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/orders/42?token=secret", nil)
	context.Header("Content-Encoding", "gzip")
	context.Header("Content-Length", "999")
	context.Header("ETag", "secret")

	AbortProblem(context, NewProblem(http.StatusForbidden, "forbidden"))

	assert.Equal(t, http.StatusForbidden, recorder.Code)
	assert.Equal(t, problemContentType, recorder.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	assert.Empty(t, recorder.Header().Get("Content-Encoding"))
	assert.Empty(t, recorder.Header().Get("Content-Length"))
	assert.Empty(t, recorder.Header().Get("ETag"))
	assert.True(t, context.IsAborted())

	var problem ProblemDetail
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &problem))
	assert.Equal(t, ProblemDetail{
		Type:       "about:blank",
		Title:      "Forbidden",
		Status:     http.StatusForbidden,
		Instance:   "/orders/42",
		Properties: map[string]any{"code": "forbidden"},
	}, problem)
	assert.NotContains(t, recorder.Body.String(), "secret")
}

func TestProblemDetailFlattensPropertiesWithoutOverridingStandardMembers(t *testing.T) {
	problem := NewProblem(http.StatusUnprocessableEntity, "invalid_order")
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
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	WriteProblem(context, ProblemDetail{Status: 0})
	WriteProblem(context, NewProblem(http.StatusBadRequest, "late"))

	assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	assert.NotContains(t, recorder.Body.String(), "late")
}

func TestWriteProblemFallsBackWhenAnExtensionCannotBeEncoded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/orders", nil)
	problem := NewProblem(http.StatusBadRequest, "bad_extension")
	problem.Properties["not_json"] = math.Inf(1)

	WriteProblem(context, problem)

	assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	var response ProblemDetail
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, "internal_server_error", response.Properties["code"])
	assert.Equal(t, "/orders", response.Instance)
}
