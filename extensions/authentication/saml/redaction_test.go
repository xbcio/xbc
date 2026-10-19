package saml

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// privateKeyFragments returns encodings of the SP private key that must never
// appear in a diagnostic, log record, or metadata document.
func privateKeyFragments(t *testing.T) []string {
	t.Helper()
	pkcs1 := base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PrivateKey(spKey))
	pkcs8, err := x509.MarshalPKCS8PrivateKey(spKey)
	if err != nil {
		t.Fatalf("marshal PKCS8: %v", err)
	}
	return []string{pkcs1, base64.StdEncoding.EncodeToString(pkcs8), "PRIVATE KEY"}
}

func assertRedacted(t *testing.T, rendered string) {
	t.Helper()
	for _, fragment := range privateKeyFragments(t) {
		if strings.Contains(rendered, fragment) {
			t.Fatalf("rendering contains private key material: %q", rendered)
		}
	}
	if !strings.Contains(rendered, "[REDACTED]") {
		t.Fatalf("rendering does not mark the private key as redacted: %q", rendered)
	}
}

func TestConfigRedactsPrivateKeyForEveryFormatVerb(t *testing.T) {
	config := testConfig()
	config.SPCertificate = spCertificate
	config.SPPrivateKey = spKey
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		rendered := fmt.Sprintf(verb, config)
		assertRedacted(t, rendered)
		if !strings.Contains(rendered, config.Name) {
			t.Fatalf("%s does not render the provider name: %q", verb, rendered)
		}
	}
}

func TestProviderRedactsPrivateKeyForPointerAndValue(t *testing.T) {
	provider := newTestProvider(t, func(config *Config) {
		config.SPCertificate = spCertificate
		config.SPPrivateKey = spKey
	})
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		assertRedacted(t, fmt.Sprintf(verb, provider))
		assertRedacted(t, fmt.Sprintf(verb, *provider))
	}
	if rendered := provider.String(); !strings.Contains(rendered, "PrivateKey:[REDACTED]") {
		t.Fatalf("String does not redact the private key: %q", rendered)
	}
}

func TestNilProviderRendersAsNil(t *testing.T) {
	var provider *Provider
	if rendered := fmt.Sprintf("%v", provider); rendered != "<nil>" {
		t.Fatalf("nil provider = %q, want <nil>", rendered)
	}
	if rendered := provider.String(); rendered != "<nil>" {
		t.Fatalf("nil provider String = %q, want <nil>", rendered)
	}
}

func TestProviderSlogRedactsPrivateKey(t *testing.T) {
	provider := newTestProvider(t, func(config *Config) {
		config.SPCertificate = spCertificate
		config.SPPrivateKey = spKey
	})
	var text bytes.Buffer
	slog.New(slog.NewTextHandler(&text, nil)).Info("probe", "provider", provider)
	assertRedacted(t, text.String())

	var json bytes.Buffer
	slog.New(slog.NewJSONHandler(&json, nil)).Info("probe", "provider", provider)
	assertRedacted(t, json.String())

	var valueText bytes.Buffer
	slog.New(slog.NewTextHandler(&valueText, nil)).Info("probe", "provider", *provider)
	assertRedacted(t, valueText.String())

	// The JSON handler marshals a Provider value structurally. Its fields are
	// unexported, so the result must be an empty object: nothing leaks even
	// though slog never calls Format here.
	var valueJSON bytes.Buffer
	slog.New(slog.NewJSONHandler(&valueJSON, nil)).Info("probe", "provider", *provider)
	if !strings.Contains(valueJSON.String(), `"provider":{}`) {
		t.Fatalf("provider value JSON = %q, want an empty object", valueJSON.String())
	}
}

func TestConfigSlogRedactsPrivateKey(t *testing.T) {
	config := testConfig()
	config.SPCertificate = spCertificate
	config.SPPrivateKey = spKey
	var text bytes.Buffer
	slog.New(slog.NewTextHandler(&text, nil)).Info("probe", "config", config)
	assertRedacted(t, text.String())

	// Config implements LogValue on the value receiver, so JSON records carry
	// the redacted string rather than a marshalled struct.
	var json bytes.Buffer
	slog.New(slog.NewJSONHandler(&json, nil)).Info("probe", "config", config)
	assertRedacted(t, json.String())
	if !strings.Contains(json.String(), `"config":"saml.Config{`) {
		t.Fatalf("config value JSON = %q, want the redacted diagnostic string", json.String())
	}
}

func TestProviderDiagnosticsKeepOperationalFields(t *testing.T) {
	provider := newTestProvider(t, nil)
	rendered := provider.String()
	for _, expected := range []string{"Name:\"corp\"", "Issuer:", "SSOURL:", "EntityID:", "ACSURL:"} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("provider diagnostics are missing %q: %q", expected, rendered)
		}
	}
}

func TestErrorsDoNotCarryAssertionContent(t *testing.T) {
	provider := newTestProvider(t, nil)
	response := buildSignedResponse(t, func(options *responseOptions) {
		options.nameID = "sensitive-subject-value"
		options.attributes = []testAttribute{
			{name: "mail", values: []string{"sensitive@example.com"}},
			{name: "displayName", values: []string{"Sensitive Name"}},
		}
	})
	tampered := strings.Replace(string(response), "Sensitive Name", "Tampered Name", 1)
	_, err := Verify(t.Context(), provider, responseInput([]byte(tampered)))
	if err == nil {
		t.Fatal("tampered response was accepted")
	}
	assertNoSensitiveText(t, err.Error())
	for _, value := range []string{"sensitive-subject-value", "sensitive@example.com", "Sensitive Name", "Tampered Name"} {
		if strings.Contains(err.Error(), value) {
			t.Fatalf("error %q contains assertion content %q", err, value)
		}
	}
}
