package pprof

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() { gin.SetMode(gin.TestMode) }

func configuredHandler(t *testing.T, mutate func(*Config)) gin.HandlerFunc {
	t.Helper()
	cfg := defaultConfig()
	mutate(&cfg)
	p, err := newPlugin(cfg)
	require.NoError(t, err)
	return p.handler()
}

func invoke(handler gin.HandlerFunc, path, remote string, headers map[string]string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.RemoteAddr = remote
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	ctx.Request = request
	handler(ctx)
	return recorder
}

func TestEnabledEndpointServesProfileAndResponsesAreNotCacheable(t *testing.T) {
	handler := configuredHandler(t, func(cfg *Config) { cfg.Enabled = true })
	response := invoke(handler, "/debug/pprof/goroutine?debug=1", "127.0.0.1:4321", nil)
	assert.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	assert.Contains(t, response.Body.String(), "goroutine profile")
}

func TestDisabledHandlerReturnsNotFound(t *testing.T) {
	p, err := newPlugin(defaultConfig())
	require.NoError(t, err)
	response := invoke(p.handler(), "/debug/pprof/", "127.0.0.1:4321", nil)
	assert.Equal(t, http.StatusNotFound, response.Code)
}
