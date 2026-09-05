package elasticsearch

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizeConfigAppliesDefaultsWithoutMutatingInput(t *testing.T) {
	addresses := []string{"  http://localhost:9200/  "}
	input := Config{Addresses: addresses}

	normalized, err := normalizeConfig(input)
	if err != nil {
		t.Fatalf("normalizeConfig() error = %v", err)
	}
	if got := normalized.Addresses[0]; got != "http://localhost:9200" {
		t.Fatalf("normalized address = %q", got)
	}
	if addresses[0] != "  http://localhost:9200/  " {
		t.Fatalf("normalizeConfig mutated input address to %q", addresses[0])
	}
	defaults := defaultConfig()
	if normalized.Timeout != defaults.Timeout || normalized.Bulk != defaults.Bulk {
		t.Fatalf("normalized defaults = %+v, want timeout %s and bulk %+v", normalized.Config, defaults.Timeout, defaults.Bulk)
	}
}

func TestConfigValidateAcceptsCloudIDAndSupportedBackpressure(t *testing.T) {
	for _, policy := range []string{BackpressureBlock, BackpressureReject, " BLOCK "} {
		cfg := Config{CloudID: "deployment:ZmFrZSRodHRwOi8vZXhhbXBsZS5jb20="}
		cfg.Bulk.Backpressure = policy
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() policy %q error = %v", policy, err)
		}
	}
}

func TestConfigValidationRejectsInvalidRelationships(t *testing.T) {
	tests := []struct {
		name   string
		config Config
		want   string
	}{
		{name: "missing endpoint", config: Config{}, want: "at least one address or cloud_id"},
		{name: "address and cloud", config: Config{Addresses: []string{"http://localhost:9200"}, CloudID: "cloud"}, want: "mutually exclusive"},
		{name: "scheme", config: Config{Addresses: []string{"ftp://localhost:9200"}}, want: "http(s) URL"},
		{name: "credentials in address", config: Config{Addresses: []string{"http://user:secret@localhost:9200"}}, want: "without credentials"},
		{name: "query in address", config: Config{Addresses: []string{"http://localhost:9200?password=secret"}}, want: "query"},
		{name: "duplicate canonical address", config: Config{Addresses: []string{"http://localhost:9200", " http://localhost:9200/ "}}, want: "duplicated"},
		{name: "username only", config: Config{Addresses: []string{"http://localhost:9200"}, Username: "service"}, want: "configured together"},
		{name: "password only", config: Config{Addresses: []string{"http://localhost:9200"}, Password: "secret"}, want: "configured together"},
		{name: "authentication conflict", config: Config{Addresses: []string{"http://localhost:9200"}, APIKey: "api-secret", Username: "service", Password: "basic-secret"}, want: "mutually exclusive"},
		{name: "timeout", config: Config{Addresses: []string{"http://localhost:9200"}, Timeout: -time.Second}, want: "timeout must be positive"},
		{name: "queue", config: Config{Addresses: []string{"http://localhost:9200"}, Bulk: BulkConfig{QueueCapacity: -1}}, want: "limits must be positive"},
		{name: "flush interval", config: Config{Addresses: []string{"http://localhost:9200"}, Bulk: BulkConfig{FlushInterval: -time.Second}}, want: "limits must be positive"},
		{name: "backpressure", config: Config{Addresses: []string{"http://localhost:9200"}, Bulk: BulkConfig{Backpressure: "drop"}}, want: "unsupported bulk backpressure"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.config.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.want)
			}
			for _, secret := range []string{"api-secret", "basic-secret", "user:secret", "password=secret"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("Validate() error exposed secret %q: %v", secret, err)
				}
			}
		})
	}
}
