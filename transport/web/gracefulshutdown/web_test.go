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

func TestLoopbackEndpointRequestsShutdown(t *testing.T) {
	p, _ := initializedPlugin(t, func(cfg *Config) { cfg.HTTP.Enabled = true })
	response := invokeShutdown(p, "127.0.0.1:4321", nil)
	if response.Code != http.StatusAccepted || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestRemoteEndpointRequiresConstantTimeTokenMatch(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	p, _ := initializedPlugin(t, func(cfg *Config) {
		cfg.HTTP.Enabled = true
		cfg.HTTP.AllowLoopback = false
		cfg.HTTP.Token = token
	})

	for name, headers := range map[string]map[string]string{
		"missing": nil,
		"wrong":   {defaultHeader: "0123456789abcdef0123456789abcdeg"},
	} {
		t.Run(name, func(t *testing.T) {
			response := invokeShutdown(p, "198.51.100.10:4321", headers)
			assertShutdownProblem(t, response, http.StatusUnauthorized, "unauthorized")
		})
	}

	response := invokeShutdown(p, "198.51.100.10:4321", map[string]string{defaultHeader: token})
	if response.Code != http.StatusAccepted {
		t.Fatalf("valid token response = %d %s", response.Code, response.Body.String())
	}
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

func TestForwardedLoopbackDoesNotAuthorizeRemotePeer(t *testing.T) {
	p, _ := initializedPlugin(t, func(cfg *Config) { cfg.HTTP.Enabled = true })
	response := invokeShutdown(p, "198.51.100.10:4321", map[string]string{"X-Forwarded-For": "127.0.0.1"})
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("forwarded loopback authorized remote peer: %d", response.Code)
	}
}

func TestDisabledEndpointFailsClosedIfHandlerInvoked(t *testing.T) {
	p, _ := initializedPlugin(t, nil)
	response := invokeShutdown(p, "127.0.0.1:4321", nil)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("disabled endpoint handler status = %d", response.Code)
	}
}

func TestDirectPeerLoopbackParsing(t *testing.T) {
	for _, address := range []string{"127.0.0.1:80", "[::1]:80", "::1"} {
		if !directPeerIsLoopback(address) {
			t.Fatalf("%q was not recognized as loopback", address)
		}
	}
	for _, address := range []string{"198.51.100.1:80", "localhost:80", ""} {
		if directPeerIsLoopback(address) {
			t.Fatalf("%q was recognized as loopback", address)
		}
	}
}
