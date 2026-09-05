package jwt

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	jwtlib "github.com/golang-jwt/jwt/v5"

	"github.com/xbcio/xbc/transport/web"
)

const currentRouteKeyForTest = "xbc/web.currentRoute"

func init() {
	gin.SetMode(gin.TestMode)
}

func requestThroughJWT(p *Plugin, method, routePath, requestPath, authorization string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	return requestThroughRouteInfo(p, web.RouteInfo{Method: method, Path: routePath}, requestPath, authorization, handler)
}

func requestThroughRouteInfo(p *Plugin, route web.RouteInfo, requestPath, authorization string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		// This is the value written by Web's outermost frozen-route middleware.
		// The JWT handler itself can only obtain it through web.CurrentRoute.
		c.Set(currentRouteKeyForTest, route)
		c.Next()
	})
	engine.Use(p.Handler())
	engine.Handle(route.Method, route.Path, handler)

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(route.Method, requestPath, nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	engine.ServeHTTP(recorder, req)
	return recorder
}

func TestPublicRouteIsResolvedAtRequestTimeAndAllowed(t *testing.T) {
	p := configuredPlugin(t, nil)
	handler := func(c *gin.Context) { c.Status(http.StatusNoContent) }

	privateRoute := web.RouteInfo{Method: http.MethodGet, Path: "/public"}
	first := requestThroughRouteInfo(p, privateRoute, "/public", "", handler)
	if first.Code != http.StatusUnauthorized {
		t.Fatalf("private request status = %d, want 401", first.Code)
	}

	// The middleware handler is already constructed. A subsequent request sees
	// that request's frozen RouteInfo.Auth value, proving Handler did not
	// capture a whitelist before route registration finished.
	publicRoute := privateRoute
	publicPolicy := web.Public()
	publicRoute.Auth = &publicPolicy
	second := requestThroughRouteInfo(p, publicRoute, "/public", "", handler)
	if second.Code != http.StatusNoContent {
		t.Fatalf("public request status = %d, want 204", second.Code)
	}
}

func TestPrivateRouteValidTokenPublishesClaimsAndSubject(t *testing.T) {
	p := configuredPlugin(t, nil)
	token, err := p.Sign("alice", Claims{"role": "admin"})
	if err != nil {
		t.Fatal(err)
	}

	response := requestThroughJWT(p, http.MethodGet, "/private", "/private", "Bearer "+token, func(c *gin.Context) {
		claims, ok := ClaimsFromContext(c)
		if !ok || claims["role"] != "admin" {
			c.Status(http.StatusInternalServerError)
			return
		}
		subject, ok := SubjectFromContext(c)
		if !ok || subject != "alice" {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.Status(http.StatusNoContent)
	})
	if response.Code != http.StatusNoContent {
		t.Fatalf("valid private request status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestPrivateRouteRejectsMissingExpiredMissingExpAndWrongAlgorithm(t *testing.T) {
	p := configuredPlugin(t, func(cfg *Config) {
		cfg.Algorithms = []string{"HS256"}
	})

	expired := signTestToken(t, jwtlib.SigningMethodHS256, jwtlib.MapClaims{
		"sub": "alice",
		"exp": jwtlib.NewNumericDate(fixedNow.Add(-time.Minute)),
	})
	missingExp := signTestToken(t, jwtlib.SigningMethodHS256, jwtlib.MapClaims{"sub": "alice"})
	wrongAlgorithm := signTestToken(t, jwtlib.SigningMethodHS512, jwtlib.MapClaims{
		"sub": "alice",
		"exp": jwtlib.NewNumericDate(fixedNow.Add(time.Hour)),
	})
	noneToken := jwtlib.NewWithClaims(jwtlib.SigningMethodNone, jwtlib.MapClaims{
		"exp": jwtlib.NewNumericDate(fixedNow.Add(time.Hour)),
	})
	none, err := noneToken.SignedString(jwtlib.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaToken := jwtlib.NewWithClaims(jwtlib.SigningMethodRS256, jwtlib.MapClaims{
		"sub": "alice",
		"exp": jwtlib.NewNumericDate(fixedNow.Add(time.Hour)),
	})
	rsaSigned, err := rsaToken.SignedString(rsaKey)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		header string
	}{
		{name: "missing token"},
		{name: "expired", header: "Bearer " + expired},
		{name: "missing exp", header: "Bearer " + missingExp},
		{name: "wrong algorithm", header: "Bearer " + wrongAlgorithm},
		{name: "none algorithm", header: "Bearer " + none},
		{name: "asymmetric algorithm confusion", header: "Bearer " + rsaSigned},
		{name: "malformed", header: "Bearer not-a-token"},
		{name: "wrong scheme", header: "Basic " + expired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := requestThroughJWT(p, http.MethodGet, "/private", "/private", tt.header, func(c *gin.Context) {
				c.Status(http.StatusNoContent)
			})
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", response.Code)
			}
			if got := response.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Fatalf("WWW-Authenticate = %q, want Bearer", got)
			}
			var body web.ProblemDetail
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("401 body is not JSON: %v", err)
			}
			if body.Status != http.StatusUnauthorized || body.Properties["code"] != "unauthorized" || body.Instance != "/private" ||
				!strings.HasPrefix(response.Header().Get("Content-Type"), "application/problem+json") ||
				strings.Contains(response.Body.String(), "token") || strings.Contains(response.Body.String(), "expired") {
				t.Fatalf("401 leaked authentication detail: %s", response.Body.String())
			}
			if tt.header != "" && strings.Contains(response.Body.String(), strings.TrimPrefix(tt.header, "Bearer ")) {
				t.Fatal("401 body leaked token")
			}
		})
	}
}

func TestExcludeIsEmergencyOverrideForTemplatePathAndMethod(t *testing.T) {
	p := configuredPlugin(t, func(cfg *Config) {
		cfg.Exclude = []string{"/health", "POST /login", "GET /users/:id"}
	})

	tests := []struct {
		name        string
		method      string
		routePath   string
		requestPath string
		want        int
	}{
		{name: "path override", method: http.MethodGet, routePath: "/health", requestPath: "/health", want: http.StatusNoContent},
		{name: "method override", method: http.MethodPost, routePath: "/login", requestPath: "/login", want: http.StatusNoContent},
		{name: "method does not match", method: http.MethodGet, routePath: "/login", requestPath: "/login", want: http.StatusUnauthorized},
		{name: "frozen template path", method: http.MethodGet, routePath: "/users/:id", requestPath: "/users/42", want: http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := requestThroughJWT(p, tt.method, tt.routePath, tt.requestPath, "", func(c *gin.Context) {
				c.Status(http.StatusNoContent)
			})
			if response.Code != tt.want {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, tt.want, response.Body.String())
			}
		})
	}
}

func TestCustomHeaderAndScheme(t *testing.T) {
	p := configuredPlugin(t, func(cfg *Config) {
		cfg.Header = "X-Access-Token"
		cfg.Scheme = "JWT"
	})
	token, err := p.Sign("alice", nil)
	if err != nil {
		t.Fatal(err)
	}

	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(currentRouteKeyForTest, web.RouteInfo{Method: http.MethodGet, Path: "/private"})
		c.Next()
	})
	engine.Use(p.Handler())
	engine.GET("/private", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/private", nil)
	req.Header.Set("X-Access-Token", "jwt "+token)
	engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("custom header request status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func signTestToken(t *testing.T, method jwtlib.SigningMethod, claims jwtlib.MapClaims) string {
	t.Helper()
	raw, err := jwtlib.NewWithClaims(method, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign test token: %v", err)
	}
	return raw
}

func TestValidTokenPublishesSharedWebPrincipal(t *testing.T) {
	p := configuredPlugin(t, nil)
	token, err := p.Sign("alice", Claims{"role": "admin"})
	if err != nil {
		t.Fatal(err)
	}

	response := requestThroughRouteInfo(p, web.RouteInfo{Method: http.MethodGet, Path: "/private"}, "/private", "Bearer "+token, func(c *gin.Context) {
		principal, ok := web.CurrentPrincipal(c)
		if !ok || principal.Subject != "alice" || principal.AuthMethod != "jwt" || principal.Attributes["role"] != "admin" {
			t.Fatalf("shared principal = %#v, ok=%v", principal, ok)
		}
		c.Status(http.StatusNoContent)
	})
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
}
