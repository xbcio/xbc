package httpclient

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizationFillsTheDocumentedDefaults(t *testing.T) {
	normalized, err := Config{}.normalized()
	require.NoError(t, err)
	assert.Equal(t, 10*time.Second, normalized.Timeout)
	assert.Equal(t, 10*time.Second, normalized.TotalTimeout, "total_timeout defaults to timeout")
	assert.Equal(t, 1, normalized.Retry.MaxAttempts, "retrying is off by default")
	assert.Equal(t, 500*time.Millisecond, normalized.Retry.InitialBackoff)
	assert.Equal(t, 30*time.Second, normalized.Retry.MaxBackoff)
	assert.Equal(t, 30*time.Second, normalized.Retry.MaxRetryAfter)
	assert.Equal(t, "1.2", normalized.TLS.MinVersion)
	assert.Equal(t, 2, normalized.MaxIdleConnsPerHost)
	assert.Equal(t, 90*time.Second, normalized.IdleConnTimeout)

	// DefaultConfig and normalization agree, so a bound configuration and a
	// constructed one behave identically.
	fromDefaults, err := DefaultConfig().normalized()
	require.NoError(t, err)
	assert.Equal(t, normalized, fromDefaults)
}

func TestNormalizationKeepsAnExplicitTotalTimeout(t *testing.T) {
	normalized, err := Config{Timeout: time.Second, TotalTimeout: 30 * time.Second}.normalized()
	require.NoError(t, err)
	assert.Equal(t, time.Second, normalized.Timeout)
	assert.Equal(t, 30*time.Second, normalized.TotalTimeout)
}

func TestValidationRejectsInvalidConfigurations(t *testing.T) {
	for _, tc := range []struct {
		name    string
		config  Config
		contain string
	}{
		{"negative timeout", Config{Timeout: -time.Second}, "timeout"},
		{"negative total timeout", Config{Timeout: time.Second, TotalTimeout: -time.Second}, "total_timeout"},
		{"relative base url", Config{BaseURL: "gateway.example/callback"}, "base_url"},
		{"unsupported scheme", Config{BaseURL: "ftp://gateway.example"}, "not supported"},
		{"missing host", Config{BaseURL: "https:///callback"}, "host"},
		{"padded base url", Config{BaseURL: " https://gateway.example"}, "base_url"},
		{"negative attempts", Config{Retry: RetryConfig{MaxAttempts: -1}}, "max_attempts"},
		{"max below initial", Config{Retry: RetryConfig{MaxAttempts: 2, InitialBackoff: time.Second, MaxBackoff: time.Millisecond}}, "max_backoff"},
		{"unsupported version", Config{TLS: TLSConfig{MinVersion: "1.0"}}, "min_version"},
		{"cert without key", Config{TLS: TLSConfig{CertFile: "client.pem"}}, "cert_file"},
		{"key without cert", Config{TLS: TLSConfig{KeyFile: "client.key"}}, "cert_file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.config.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.contain)
		})
	}
}

func TestValidationAcceptsTheDefaultConfiguration(t *testing.T) {
	assert.NoError(t, Config{}.Validate())
	assert.NoError(t, Config{BaseURL: "https://gateway.example:8443/root"}.Validate())
	assert.NoError(t, Config{TLS: TLSConfig{MinVersion: "1.3"}}.Validate())
}

func TestEndpointRendersSchemeAndHostOnly(t *testing.T) {
	for _, tc := range []struct {
		baseURL string
		want    string
	}{
		{"", ""},
		{"https://gateway.example", "https://gateway.example"},
		{"https://gateway.example:8443/v1/call?tenant=42", "https://gateway.example:8443"},
		{"http://127.0.0.1:9090", "http://127.0.0.1:9090"},
	} {
		config := Config{BaseURL: tc.baseURL}
		assert.Equal(t, tc.want, config.endpoint(), "base_url %q", tc.baseURL)
	}
}

func TestMinVersionResolvesTheTwoSupportedFloors(t *testing.T) {
	if version, err := (TLSConfig{}).minVersion(); err != nil || version == 0 {
		t.Fatalf("empty min_version must resolve to a floor, got %v, %v", version, err)
	}
	if _, err := (TLSConfig{MinVersion: "1.4"}).minVersion(); err == nil {
		t.Fatal("an unsupported TLS floor must be rejected")
	}
	for _, raw := range []string{"1.2", "1.3"} {
		assert.NoError(t, Config{TLS: TLSConfig{MinVersion: raw}}.Validate(), raw)
	}
	assert.True(t, strings.HasPrefix(Config{TLS: TLSConfig{MinVersion: "1.4"}}.Validate().Error(), "http-client:"))
}
