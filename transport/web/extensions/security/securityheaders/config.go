package securityheaders

import (
	"fmt"
	"strings"
)

const (
	defaultCSP               = "default-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'self'"
	defaultPermissionsPolicy = "camera=(), geolocation=(), microphone=()"
)

// Config is bound from plugins.securityheaders. Empty configurable policy
// strings disable that individual header.
type Config struct {
	ContentTypeNosniff      bool   `yaml:"content_type_nosniff"       default:"true"`
	FrameOptions            string `yaml:"frame_options"              default:"DENY"`
	ContentSecurityPolicy   string `yaml:"content_security_policy"    default:"default-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'self'"`
	ReferrerPolicy          string `yaml:"referrer_policy"            default:"no-referrer"`
	PermissionsPolicy       string `yaml:"permissions_policy"         default:"camera=(), geolocation=(), microphone=()"`
	CrossOriginOpenerPolicy string `yaml:"cross_origin_opener_policy" default:"same-origin"`
	XSSProtection           string `yaml:"xss_protection"             default:"0"`
	HSTSEnabled             bool   `yaml:"hsts_enabled"               default:"true"`
	HSTSMaxAge              int    `yaml:"hsts_max_age"               default:"31536000" validate:"min=0,max=63072000"`
	HSTSIncludeSubdomains   bool   `yaml:"hsts_include_subdomains"    default:"true"`
	HSTSPreload             bool   `yaml:"hsts_preload"               default:"false"`
	HSTSOnlyHTTPS           bool   `yaml:"hsts_only_https"            default:"true"`
	HSTSTrustForwardedProto bool   `yaml:"hsts_trust_forwarded_proto" default:"false"`
}

// DefaultConfig returns the same defaults applied by XBC's config binder.
func DefaultConfig() Config {
	return Config{
		ContentTypeNosniff:      true,
		FrameOptions:            "DENY",
		ContentSecurityPolicy:   defaultCSP,
		ReferrerPolicy:          "no-referrer",
		PermissionsPolicy:       defaultPermissionsPolicy,
		CrossOriginOpenerPolicy: "same-origin",
		XSSProtection:           "0",
		HSTSEnabled:             true,
		HSTSMaxAge:              31536000,
		HSTSIncludeSubdomains:   true,
		HSTSOnlyHTTPS:           true,
	}
}

// Validate rejects malformed or injectable header values and inconsistent
// preload settings.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

type headerValue struct{ name, value string }

type normalizedConfig struct {
	headers                 []headerValue
	hsts                    string
	hstsOnlyHTTPS           bool
	hstsTrustForwardedProto bool
}

func normalizeConfig(c Config) (normalizedConfig, error) {
	if c.HSTSMaxAge < 0 || c.HSTSMaxAge > 63072000 {
		return normalizedConfig{}, fmt.Errorf("securityheaders: hsts_max_age must be between 0 and 63072000")
	}
	if c.HSTSPreload && (!c.HSTSEnabled || c.HSTSMaxAge < 31536000 || !c.HSTSIncludeSubdomains) {
		return normalizedConfig{}, fmt.Errorf("securityheaders: hsts_preload requires HSTS enabled, max age >= 31536000, and include_subdomains")
	}
	values := []headerValue{
		{name: "X-Frame-Options", value: c.FrameOptions},
		{name: "Content-Security-Policy", value: c.ContentSecurityPolicy},
		{name: "Referrer-Policy", value: c.ReferrerPolicy},
		{name: "Permissions-Policy", value: c.PermissionsPolicy},
		{name: "Cross-Origin-Opener-Policy", value: c.CrossOriginOpenerPolicy},
		{name: "X-XSS-Protection", value: c.XSSProtection},
	}
	for i := range values {
		if containsControlCharacter(values[i].value) {
			return normalizedConfig{}, fmt.Errorf("securityheaders: %s contains a forbidden control character", values[i].name)
		}
		values[i].value = strings.TrimSpace(values[i].value)
	}
	if value := values[0].value; value != "" && value != "DENY" && value != "SAMEORIGIN" {
		return normalizedConfig{}, fmt.Errorf("securityheaders: frame_options must be DENY, SAMEORIGIN, or empty")
	}
	if c.ContentTypeNosniff {
		values = append(values, headerValue{name: "X-Content-Type-Options", value: "nosniff"})
	}
	hsts := ""
	if c.HSTSEnabled {
		hsts = fmt.Sprintf("max-age=%d", c.HSTSMaxAge)
		if c.HSTSIncludeSubdomains {
			hsts += "; includeSubDomains"
		}
		if c.HSTSPreload {
			hsts += "; preload"
		}
	}
	return normalizedConfig{
		headers:                 values,
		hsts:                    hsts,
		hstsOnlyHTTPS:           c.HSTSOnlyHTTPS,
		hstsTrustForwardedProto: c.HSTSTrustForwardedProto,
	}, nil
}

func containsControlCharacter(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] == 0x7f {
			return true
		}
	}
	return false
}
