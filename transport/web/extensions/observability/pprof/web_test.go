package pprof

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
)

func init() { gin.SetMode(gin.TestMode) }

func configuredHandler(t *testing.T, mutate func(*Config)) web.Handler {
	t.Helper()
	cfg := defaultConfig()
	mutate(&cfg)
	p, err := newPlugin(cfg)
	require.NoError(t, err)
	return p.handler()
}

func invoke(handler web.Handler, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, path, nil)
	web.Handle(handler)(ctx)
	return recorder
}

func TestEnabledEndpointServesProfileAndResponsesAreNotCacheable(t *testing.T) {
	handler := configuredHandler(t, func(cfg *Config) { cfg.Enabled = true })
	response := invoke(handler, "/debug/pprof/goroutine?debug=1")
	assert.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	assert.Contains(t, response.Body.String(), "goroutine profile")
}

func TestDisabledHandlerReturnsNotFound(t *testing.T) {
	p, err := newPlugin(defaultConfig())
	require.NoError(t, err)
	response := invoke(p.handler(), "/debug/pprof/")
	assert.Equal(t, http.StatusNotFound, response.Code)
	assert.Equal(t, "application/problem+json; charset=utf-8", response.Header().Get("Content-Type"))
	var problem web.ProblemDetail
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
	assert.Equal(t, http.StatusNotFound, problem.Status)
	assert.Equal(t, "not_found", problem.Properties["code"])
	assert.Equal(t, "/debug/pprof/", problem.Instance)
}
