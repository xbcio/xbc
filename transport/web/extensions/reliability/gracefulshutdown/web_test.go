package gracefulshutdown

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

func invokeShutdown(p *Plugin, remoteAddr string, headers map[string]string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/-/shutdown", nil)
	request.RemoteAddr = remoteAddr
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	ctx.Request = request
	p.handleShutdown(ctx)
	return recorder
}

func TestEndpointRequestsShutdown(t *testing.T) {
	p, _ := initializedPlugin(t, func(cfg *Config) { cfg.HTTP.Enabled = true })
	response := invokeShutdown(p, "127.0.0.1:4321", nil)
	if response.Code != http.StatusAccepted || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestDisabledEndpointReturnsNotFound(t *testing.T) {
	p, _ := initializedPlugin(t, nil)
	response := invokeShutdown(p, "127.0.0.1:4321", nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("disabled endpoint handler status = %d", response.Code)
	}
}
