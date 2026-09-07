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

func TestLoopbackCanReadProfileAndResponsesAreNotCacheable(t *testing.T) {
	handler := configuredHandler(t, func(cfg *Config) { cfg.Enabled = true })
	response := invoke(handler, "/debug/pprof/goroutine?debug=1", "127.0.0.1:4321", nil)
	assert.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	assert.Contains(t, response.Body.String(), "goroutine profile")
}

func TestRemotePeerRequiresTokenAndForwardedLoopbackIsIgnored(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	handler := configuredHandler(t, func(cfg *Config) {
		cfg.Enabled = true
		cfg.AllowLoopback = false
		cfg.Token = token
	})

	for name, headers := range map[string]map[string]string{
		"missing":   nil,
		"wrong":     {defaultHeader: "0123456789abcdef0123456789abcdeg"},
		"forwarded": {"X-Forwarded-For": "127.0.0.1"},
	} {
		t.Run(name, func(t *testing.T) {
			response := invoke(handler, "/debug/pprof/", "198.51.100.10:4321", headers)
			assert.Equal(t, http.StatusUnauthorized, response.Code)
			assert.Equal(t, "application/problem+json; charset=utf-8", response.Header().Get("Content-Type"))
			var problem web.ProblemDetail
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
			assert.Equal(t, http.StatusUnauthorized, problem.Status)
			assert.Equal(t, "unauthorized", problem.Properties["code"])
			assert.Equal(t, "/debug/pprof/", problem.Instance)
		})
	}

	response := invoke(handler, "/debug/pprof/", "198.51.100.10:4321", map[string]string{defaultHeader: token})
	assert.Equal(t, http.StatusOK, response.Code)
	assert.Contains(t, response.Body.String(), "profile")
}

func TestDisabledHandlerFailsClosed(t *testing.T) {
	p, err := newPlugin(defaultConfig())
	require.NoError(t, err)
	response := invoke(p.handler(), "/debug/pprof/", "127.0.0.1:4321", nil)
	assert.Equal(t, http.StatusUnauthorized, response.Code)
}
