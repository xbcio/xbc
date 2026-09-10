package gracefulshutdown

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

func init() { gin.SetMode(gin.TestMode) }

func invokeShutdown(p *Plugin) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/-/shutdown", nil)
	p.handleShutdown(ctx)
	return recorder
}

func assertShutdownProblem(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/problem+json") {
		t.Fatalf("response = %d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
	var problem web.ProblemDetail
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Status != status || problem.Properties["code"] != code || problem.Instance != "/-/shutdown" {
		t.Fatalf("problem = %#v", problem)
	}
}

func TestEndpointRequestsShutdown(t *testing.T) {
	p, _ := initializedPlugin(t, func(cfg *Config) { cfg.HTTP.Enabled = true })
	response := invokeShutdown(p)
	if response.Code != http.StatusAccepted || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestDisabledEndpointReturnsNotFound(t *testing.T) {
	p, _ := initializedPlugin(t, nil)
	assertShutdownProblem(t, invokeShutdown(p), http.StatusNotFound, "not_found")
}
