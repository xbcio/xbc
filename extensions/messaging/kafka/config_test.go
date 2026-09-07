package kafka

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizeConfigAppliesDefaultsWithoutMutatingCredentials(t *testing.T) {
	cfg := validConfig()
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatalf("normalizeConfig() error = %v", err)
	}
	if normalized.ClientID != "xbc" || normalized.DialTimeout != 10*time.Second {
		t.Fatalf("connection defaults = %q, %s", normalized.ClientID, normalized.DialTimeout)
	}
	if normalized.Producer.BatchSize != 100 || normalized.Producer.RequiredAcks != AckAll || normalized.Producer.Compression != CompressionNone {
		t.Fatalf("producer defaults = %+v", normalized.Producer)
	}

	cfg.Consumers = map[string]ConsumerConfig{"jobs": {GroupID: "group", Topics: []string{"topic"}}}
	normalized, err = normalizeConfig(cfg)
	if err != nil {
		t.Fatalf("normalizeConfig(consumer) error = %v", err)
	}
	consumer := normalized.consumers["jobs"]
	if consumer.QueueCapacity != 100 || consumer.MinBytes != 1 || consumer.MaxBytes != 10<<20 || consumer.BalanceStrategy != BalanceRange {
		t.Fatalf("consumer defaults = %+v", consumer)
	}
	if consumer.StartOffset != OffsetEarliest || consumer.ErrorPolicy != ErrorPolicyStop {
		t.Fatalf("consumer delivery defaults = %+v", consumer)
	}
}

func TestConfigValidationAndSecretSafety(t *testing.T) {
	const secret = "never-render-this-password"
	tests := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{name: "brokers required", edit: func(c *Config) { c.Brokers = nil }, want: "at least one broker"},
		{name: "broker host port", edit: func(c *Config) { c.Brokers = []string{"localhost"} }, want: "host:port"},
		{name: "duplicate broker", edit: func(c *Config) { c.Brokers = []string{"a:1", "a:1"} }, want: "duplicated"},
		{name: "tls key pair", edit: func(c *Config) { c.TLS.CertFile = "client.pem" }, want: "configured together"},
		{name: "sasl requires tls", edit: func(c *Config) { c.SASL = SASLConfig{Mechanism: SASLPlain, Username: "u", Password: secret} }, want: "requires tls"},
		{name: "sasl mechanism", edit: func(c *Config) {
			c.TLS.Enabled = true
			c.SASL = SASLConfig{Mechanism: "bad", Username: "u", Password: secret}
		}, want: "unsupported sasl"},
		{name: "producer acks", edit: func(c *Config) { c.Producer.RequiredAcks = "sometimes" }, want: "required_acks"},
		{name: "producer compression", edit: func(c *Config) { c.Producer.Compression = "zip" }, want: "compression"},
		{name: "consumer bytes", edit: func(c *Config) {
			c.Consumers = map[string]ConsumerConfig{"c": consumerConfig(ErrorPolicyStop)}
			value := c.Consumers["c"]
			value.MinBytes = 20
			value.MaxBytes = 10
			c.Consumers["c"] = value
		}, want: "byte limits"},
		{name: "consumer policy", edit: func(c *Config) { c.Consumers = map[string]ConsumerConfig{"c": consumerConfig("discard")} }, want: "error_policy"},
		{name: "consumer strategy", edit: func(c *Config) {
			c.Consumers = map[string]ConsumerConfig{"c": consumerConfig(ErrorPolicyStop)}
			value := c.Consumers["c"]
			value.BalanceStrategy = "sticky"
			c.Consumers["c"] = value
		}, want: "balance_strategy"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validConfig()
			test.edit(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("Validate() leaked SASL password: %v", err)
			}
		})
	}
}

func TestBackendMappings(t *testing.T) {
	if requiredAcks(AckNone) != 0 || requiredAcks(AckOne) != 1 || requiredAcks(AckAll) != -1 {
		t.Fatal("requiredAcks mapping is incorrect")
	}
	for _, value := range []string{CompressionNone, CompressionGzip, CompressionSnappy, CompressionLZ4, CompressionZstd} {
		if _, err := compressionCodec(value); err != nil {
			t.Fatalf("compressionCodec(%q) error = %v", value, err)
		}
	}
	if _, err := compressionCodec("bad"); err == nil {
		t.Fatal("compressionCodec(bad) error = nil")
	}
	if groupBalancer(BalanceRange).ProtocolName() != "range" || groupBalancer(BalanceRoundRobin).ProtocolName() != "roundrobin" {
		t.Fatal("group balancer mapping is incorrect")
	}
}
