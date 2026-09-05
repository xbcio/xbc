package securityheaders

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/cors"
	"github.com/xbcio/xbc/transport/web/ratelimit"
)

func TestDefinitionAndPhaseContract(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero || Definition() != Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	middleware := New()
	order := middleware.Order()
	if order.Phase != web.PhaseSecurity || len(order.Before) != 2 || len(order.After) != 0 || middleware.Handler() == nil {
		t.Fatalf("unexpected middleware order: %#v", order)
	}
	want := []plugin.Key{cors.Key, ratelimit.Key}
	for index, ref := range order.Before {
		if ref.Key() != want[index] || ref.InstanceName() != "" || ref.Required() {
			t.Fatalf("order.Before[%d] = %#v, want optional %s key", index, ref, want[index])
		}
	}
}

func TestSafeDefaultsAndHTTPSOnlyHSTS(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	p := New()
	router := gin.New()
	router.Use(p.handle)
	router.GET("/", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	httpResponse := httptest.NewRecorder()
	router.ServeHTTP(httpResponse, httptest.NewRequest(http.MethodGet, "/", nil))
	for header, want := range map[string]string{
		"X-Content-Type-Options":     "nosniff",
		"X-Frame-Options":            "DENY",
		"Content-Security-Policy":    defaultCSP,
		"Referrer-Policy":            "no-referrer",
		"Permissions-Policy":         defaultPermissionsPolicy,
		"Cross-Origin-Opener-Policy": "same-origin",
		"X-XSS-Protection":           "0",
	} {
		if got := httpResponse.Header().Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	if got := httpResponse.Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("HSTS sent over plaintext: %q", got)
	}

	httpsRequest := httptest.NewRequest(http.MethodGet, "https://example.test/", nil)
	httpsRequest.TLS = &tls.ConnectionState{}
	httpsResponse := httptest.NewRecorder()
	router.ServeHTTP(httpsResponse, httpsRequest)
	if got := httpsResponse.Header().Get("Strict-Transport-Security"); got != "max-age=31536000; includeSubDomains" {
		t.Fatalf("HSTS = %q", got)
	}
}

func TestConfigurationDisablesPoliciesAndRejectsInjection(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ContentSecurityPolicy = ""
	cfg.FrameOptions = ""
	cfg.HSTSEnabled = false
	state, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := New()
	p.state.Store(&state)
	router := gin.New()
	router.Use(p.handle)
	router.GET("/", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	for _, header := range []string{"Content-Security-Policy", "X-Frame-Options", "Strict-Transport-Security"} {
		if got := response.Header().Get(header); got != "" {
			t.Fatalf("disabled %s = %q", header, got)
		}
	}

	bad := DefaultConfig()
	bad.ContentSecurityPolicy = "default-src 'self'\r\nX-Evil: injected"
	if err := bad.Validate(); err == nil {
		t.Fatal("header injection was accepted")
	}
	for _, control := range []byte{'\t', 0x1f, 0x7f} {
		bad = DefaultConfig()
		bad.PermissionsPolicy = "camera=()" + string(control)
		if err := bad.Validate(); err == nil {
			t.Fatalf("header control byte %#x was accepted", control)
		}
	}
	preload := DefaultConfig()
	preload.HSTSPreload = true
	preload.HSTSIncludeSubdomains = false
	if err := preload.Validate(); err == nil {
		t.Fatal("unsafe HSTS preload was accepted")
	}
}
