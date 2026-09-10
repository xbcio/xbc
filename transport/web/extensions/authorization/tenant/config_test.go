package tenant

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

func TestDefinitionIsCanonicalAndBundleIsStable(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero || Definition() != Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	bundle := Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(bundle, Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}
}

func TestSecureDefaults(t *testing.T) {
	cfg := defaultConfig()
	if !cfg.Required || cfg.Header != defaultHeader || cfg.TenantIDAttribute != defaultTenantIDAttribute || cfg.TenantIDsAttribute != defaultTenantIDsAttribute || cfg.TenantAttributesAttribute != defaultAttributesAttribute || !cfg.AutoSelectSingle || cfg.MinIDLength != 1 || cfg.MaxIDLength != 128 {
		t.Fatalf("defaults=%#v", cfg)
	}
}

func TestConfigRejectsUnsafeHeadersKeysAndIDBounds(t *testing.T) {
	base := defaultConfig()
	cases := map[string]func(*Config){
		"header whitespace": func(c *Config) { c.Header = "X Tenant" },
		"header control":    func(c *Config) { c.Header = "X-Tenant\nInjected" },
		"attribute control": func(c *Config) { c.TenantIDAttribute = "tenant\x00id" },
		"duplicate keys":    func(c *Config) { c.TenantIDsAttribute = c.TenantIDAttribute },
		"minimum":           func(c *Config) { c.MinIDLength = -1 },
		"reversed bounds":   func(c *Config) { c.MinIDLength, c.MaxIDLength = 20, 10 },
		"maximum":           func(c *Config) { c.MaxIDLength = 513 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate(%#v) succeeded", cfg)
			}
		})
	}
}

func TestMiddlewareOrderingAfterAuthenticationBeforeAuthorization(t *testing.T) {
	middleware := New()
	if middleware.Handler() == nil {
		t.Fatal("Handler() returned nil")
	}
	order := middleware.Order()
	if order.Phase != web.PhaseAuth {
		t.Fatalf("Phase=%v", order.Phase)
	}
	if len(order.After) != 1 {
		t.Fatalf("After=%#v", order.After)
	}
	if ref := order.After[0]; ref.Key() != web.AuthenticationMiddlewareKey || ref.InstanceName() != "" || !ref.Required() {
		t.Fatalf("After[0]=%#v, want required %v", ref, web.AuthenticationMiddlewareKey)
	}
	if len(order.Before) != 1 {
		t.Fatalf("Before=%#v", order.Before)
	}
	if ref := order.Before[0]; ref.Key() != casbinKey || ref.InstanceName() != "" || ref.Required() {
		t.Fatalf("Before[0]=%#v, want optional %v", ref, casbinKey)
	}
}

// TestOrderRequiresAuthenticationMiddleware locks the hard-reference: a soft
// Prefer degrades silently into lexicographic tie-break the moment its target
// plugin's Order() stops naming it, and that yields a 403/401 at runtime with
// no compile or test failure to point at it. A mutation back to
// web.Prefer(web.AuthenticationMiddlewareKey) must fail this test.
func TestOrderRequiresAuthenticationMiddleware(t *testing.T) {
	t.Parallel()

	order := New().Order()
	var found bool
	for _, ref := range order.After {
		if ref == web.Require(web.AuthenticationMiddlewareKey) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("authorization must hard-require the authentication middleware; " +
			"a soft Prefer degrades silently into lexicographic tie-break and yields 403/401")
	}
}
