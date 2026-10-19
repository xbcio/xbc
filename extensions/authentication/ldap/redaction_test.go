package ldap

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// redactedMarker is what every secret-bearing view must render.
const redactedMarker = "[REDACTED]"

func TestConfigAndProviderViewsRedactTheServicePassword(t *testing.T) {
	const secret = "super-secret-service-password"
	config := testConfig("ldaps://directory.example.com")
	config.ServiceBindPassword = "super-secret-service-password"
	provider := newProvider(t, config)

	for name, rendered := range map[string]string{
		"config String":       config.String(),
		"config %v":           fmt.Sprintf("%v", config),
		"config %+v":          fmt.Sprintf("%+v", config),
		"config %#v":          fmt.Sprintf("%#v", config),
		"config %s":           fmt.Sprintf("%s", config),
		"provider String":     provider.String(),
		"provider %v":         fmt.Sprintf("%v", provider),
		"provider %+v":        fmt.Sprintf("%+v", provider),
		"provider %#v":        fmt.Sprintf("%#v", provider),
		"provider %s":         fmt.Sprintf("%s", provider),
		"nil provider String": (*Provider)(nil).String(),
	} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("%s leaks the service password: %q", name, rendered)
		}
	}
	for name, rendered := range map[string]string{
		"config String":   config.String(),
		"provider String": provider.String(),
		"config %#v":      fmt.Sprintf("%#v", config),
		"provider %#v":    fmt.Sprintf("%#v", provider),
	} {
		if !strings.Contains(rendered, redactedMarker) {
			t.Fatalf("%s does not mark the redaction: %q", name, rendered)
		}
	}
}

// TestDereferencedProviderValuesRedactTheServicePassword covers the value
// method set. fmt does not dispatch methods on unexported struct fields, so
// without a value-receiver Format a Provider value would print the embedded
// settings -- and the service password -- field by field.
func TestDereferencedProviderValuesRedactTheServicePassword(t *testing.T) {
	const secret = "super-secret-service-password"
	config := testConfig("ldaps://directory.example.com")
	config.ServiceBindPassword = "super-secret-service-password"
	provider := newProvider(t, config)
	value := *provider

	for name, rendered := range map[string]string{
		"provider value %v":  fmt.Sprintf("%v", value),
		"provider value %+v": fmt.Sprintf("%+v", value),
		"provider value %#v": fmt.Sprintf("%#v", value),
	} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("%s leaks the service password: %q", name, rendered)
		}
		if !strings.Contains(rendered, redactedMarker) {
			t.Fatalf("%s does not mark the redaction: %q", name, rendered)
		}
		// Diagnostic values are intentionally kept (see doc.go): only the
		// password is a credential.
		if !strings.Contains(rendered, config.ServiceBindDN) {
			t.Fatalf("%s dropped the diagnostic ServiceBindDN: %q", name, rendered)
		}
	}

	// The stored field keeps the outbound.Secret type, so formatting the field
	// value itself is redacted independently of Provider.Format.
	for name, rendered := range map[string]string{
		"settings password %v":  fmt.Sprintf("%v", provider.serviceBindPassword),
		"settings password %#v": fmt.Sprintf("%#v", provider.serviceBindPassword),
	} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("%s leaks the service password: %q", name, rendered)
		}
	}

	if rendered := fmt.Sprintf("%v", (*Provider)(nil)); rendered != "<nil>" {
		t.Fatalf("nil provider renders %q, want <nil>", rendered)
	}
}

func TestStructuredLogsRedactTheServicePassword(t *testing.T) {
	const secret = "super-secret-service-password"
	config := testConfig("ldaps://directory.example.com")
	config.ServiceBindPassword = "super-secret-service-password"
	provider := newProvider(t, config)
	value := *provider

	var buffer bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buffer, nil))
	logger.Info("directory configured", "config", config, "provider", provider, "provider_value", value)
	rendered := buffer.String()
	if strings.Contains(rendered, secret) {
		t.Fatalf("text log output leaks the service password: %s", rendered)
	}
	if !strings.Contains(rendered, redactedMarker) {
		t.Fatalf("text log output does not mark the redaction: %s", rendered)
	}

	buffer.Reset()
	jsonLogger := slog.New(slog.NewJSONHandler(&buffer, nil))
	jsonLogger.Info("directory configured", "config", config, "provider", provider, "provider_value", value)
	rendered = buffer.String()
	if strings.Contains(rendered, secret) {
		t.Fatalf("JSON log output leaks the service password: %s", rendered)
	}
	// Lock the two distinct JSON shapes: a *Provider is a slog.LogValuer and
	// logs the redacted string, while a Provider value is not (LogValue has a
	// pointer receiver) and encoding/json omits its unexported fields, so it
	// logs as an empty object.
	if !strings.Contains(rendered, `"provider_value":{}`) {
		t.Fatalf("JSON log output changed the Provider value shape: %s", rendered)
	}
	if !strings.Contains(rendered, redactedMarker) {
		t.Fatalf("JSON log output does not render the redacted pointer view: %s", rendered)
	}
}

func TestConfigErrorsNeverCarryTheServicePassword(t *testing.T) {
	const secret = "super-secret-service-password"
	for _, test := range []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "bad issuer", mutate: func(c *Config) { c.Issuer = "" }},
		{name: "bad URL", mutate: func(c *Config) { c.URL = "http://directory.example.com" }},
		{name: "bad DN", mutate: func(c *Config) { c.BaseDN = "broken" }},
		{name: "bad filter", mutate: func(c *Config) { c.UserFilter = "uid=%s" }},
		{name: "bad attribute", mutate: func(c *Config) { c.SubjectAttribute = "bad name" }},
		{name: "bad claim", mutate: func(c *Config) { c.ClaimAttributes = map[string]string{"email": "bad name"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig("ldaps://directory.example.com")
			config.ServiceBindPassword = "super-secret-service-password"
			test.mutate(&config)
			_, err := New(config)
			if err == nil {
				t.Fatal("New accepted an invalid configuration")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaks the service password: %v", err)
			}
		})
	}
}
