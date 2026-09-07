package apikey

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

const currentRouteKeyForTest = "xbc/web.currentRoute"

func init() { gin.SetMode(gin.TestMode) }

func configuredPlugin(t *testing.T, requireAppID bool) *Plugin {
	t.Helper()
	cfg := DefaultConfig()
	cfg.RequireAppID = requireAppID
	cfg.Static = []StaticCredential{{
		ID:      "payments",
		AppID:   "checkout",
		Subject: "service:payments",
		SHA256:  HashKey("0123456789abcdef0123456789abcdef").String(),
		Attributes: map[string]any{
			"role": "writer",
		},
	}}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func request(t *testing.T, p *Plugin, route web.RouteInfo, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Set(currentRouteKeyForTest, route); c.Next() })
	engine.Use(p.Handler())
	engine.Handle(route.Method, route.Path, func(c *gin.Context) {
		principal, ok := web.CurrentPrincipal(c)
		if route.Auth.IsPublic() {
			if ok {
				c.Status(http.StatusInternalServerError)
				return
			}
			c.Status(http.StatusNoContent)
			return
		}
		if !ok || principal.Subject != "service:payments" || principal.AuthMethod != "apikey" || principal.Attributes["credential_id"] != "payments" {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.Status(http.StatusNoContent)
	})
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(route.Method, route.Path, nil)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	engine.ServeHTTP(recorder, req)
	return recorder
}

func TestAPIKeyAndBearerAuthenticateAndPublishPrincipal(t *testing.T) {
	p := configuredPlugin(t, true)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/private"}
	for name, headers := range map[string]map[string]string{
		"api header": {"X-API-Key": "0123456789abcdef0123456789abcdef", "X-App-ID": "checkout"},
		"bearer":     {"Authorization": "Bearer 0123456789abcdef0123456789abcdef", "X-App-ID": "checkout"},
	} {
		t.Run(name, func(t *testing.T) {
			response := request(t, p, route, headers)
			if response.Code != http.StatusNoContent {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestPublicAuthPolicyBypassesAndPrivateFailuresAreUniform(t *testing.T) {
	p := configuredPlugin(t, false)
	publicPolicy := web.Public()
	public := web.RouteInfo{Method: http.MethodGet, Path: "/public", Auth: &publicPolicy}
	if got := request(t, p, public, nil).Code; got != http.StatusNoContent {
		t.Fatalf("public status = %d", got)
	}
	private := web.RouteInfo{Method: http.MethodGet, Path: "/private"}
	for name, headers := range map[string]map[string]string{
		"missing":   nil,
		"wrong":     {"X-API-Key": "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
		"ambiguous": {"X-API-Key": "0123456789abcdef0123456789abcdef", "Authorization": "Bearer 0123456789abcdef0123456789abcdef"},
	} {
		t.Run(name, func(t *testing.T) {
			response := request(t, p, private, headers)
			assertProblem(t, response, http.StatusUnauthorized, "unauthorized", "/private")
		})
	}
}

func assertProblem(t *testing.T, response *httptest.ResponseRecorder, status int, code, instance string) {
	t.Helper()
	if response.Code != status || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/problem+json") {
		t.Fatalf("response = %d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
	var problem web.ProblemDetail
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Status != status || problem.Properties["code"] != code || problem.Instance != instance {
		t.Fatalf("problem = %#v", problem)
	}
}

func TestRequireAppIDAndRotation(t *testing.T) {
	p := configuredPlugin(t, true)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/private"}
	secret := "0123456789abcdef0123456789abcdef"
	if got := request(t, p, route, map[string]string{"X-API-Key": secret}).Code; got != http.StatusUnauthorized {
		t.Fatalf("missing app ID status = %d", got)
	}
	newSecret := "abcdef0123456789abcdef0123456789"
	if err := p.ReplaceStaticCredentials([]StaticCredential{{ID: "payments", AppID: "checkout", Subject: "service:payments", SHA256: HashKey(newSecret).String()}}); err != nil {
		t.Fatal(err)
	}
	if got := request(t, p, route, map[string]string{"X-API-Key": secret, "X-App-ID": "checkout"}).Code; got != http.StatusUnauthorized {
		t.Fatalf("old key status = %d", got)
	}
	if got := request(t, p, route, map[string]string{"X-API-Key": newSecret, "X-App-ID": "checkout"}).Code; got != http.StatusNoContent {
		t.Fatalf("new key status = %d", got)
	}
}

func TestDuplicateCredentialHeadersAreRejected(t *testing.T) {
	p := configuredPlugin(t, false)
	route := web.RouteInfo{Method: http.MethodGet, Path: "/private"}
	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Set(currentRouteKeyForTest, route); c.Next() })
	engine.Use(p.Handler())
	engine.GET(route.Path, func(c *gin.Context) { c.Status(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodGet, route.Path, nil)
	req.Header.Add("X-API-Key", "0123456789abcdef0123456789abcdef")
	req.Header.Add("X-API-Key", "abcdef0123456789abcdef0123456789")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("duplicate header status = %d", recorder.Code)
	}
}
