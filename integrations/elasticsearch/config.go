package elasticsearch

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	BackpressureBlock  = "block"
	BackpressureReject = "reject"
)

// Config configures one named Elasticsearch cluster. Secrets are accepted as
// configuration values for integration with XBC secret sources; this package
// never includes them in errors or logs.
type Config struct {
	Addresses []string `yaml:"addresses"`
	CloudID   string   `yaml:"cloud_id"`
	APIKey    string   `yaml:"api_key"`
	Username  string   `yaml:"username"`
	Password  string   `yaml:"password"`

	TLS         TLSConfig     `yaml:"tls"`
	Timeout     time.Duration `yaml:"timeout" default:"10s" validate:"gt=0"`
	HealthProbe bool          `yaml:"health_probe" default:"true"`
	Bulk        BulkConfig    `yaml:"bulk"`
}

// TLSConfig optionally supplies PEM certificate authorities for HTTPS nodes.
type TLSConfig struct {
	CAFile string `yaml:"ca_file"`
}

// BulkConfig bounds memory and controls automatic asynchronous flushes.
type BulkConfig struct {
	QueueCapacity int           `yaml:"queue_capacity" default:"1000" validate:"gt=0"`
	Backpressure  string        `yaml:"backpressure" default:"block" validate:"oneof=block reject"`
	FlushInterval time.Duration `yaml:"flush_interval" default:"1s" validate:"gt=0"`
	FlushActions  int           `yaml:"flush_actions" default:"500" validate:"gt=0"`
	FlushBytes    int           `yaml:"flush_bytes" default:"5242880" validate:"gt=0"`
}

type normalizedConfig struct{ Config }

func defaultConfig() Config {
	return Config{
		Timeout:     10 * time.Second,
		HealthProbe: true,
		Bulk: BulkConfig{
			QueueCapacity: 1000,
			Backpressure:  BackpressureBlock,
			FlushInterval: time.Second,
			FlushActions:  500,
			FlushBytes:    5 << 20,
		},
	}
}

// Validate applies defaults and validates semantic relationships without
// making network or filesystem calls.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

func normalizeConfig(c Config) (normalizedConfig, error) {
	d := defaultConfig()
	if c.Timeout == 0 {
		c.Timeout = d.Timeout
	}
	if c.Bulk.QueueCapacity == 0 {
		c.Bulk.QueueCapacity = d.Bulk.QueueCapacity
	}
	if strings.TrimSpace(c.Bulk.Backpressure) == "" {
		c.Bulk.Backpressure = d.Bulk.Backpressure
	}
	if c.Bulk.FlushInterval == 0 {
		c.Bulk.FlushInterval = d.Bulk.FlushInterval
	}
	if c.Bulk.FlushActions == 0 {
		c.Bulk.FlushActions = d.Bulk.FlushActions
	}
	if c.Bulk.FlushBytes == 0 {
		c.Bulk.FlushBytes = d.Bulk.FlushBytes
	}
	c.CloudID = strings.TrimSpace(c.CloudID)
	c.Username = strings.TrimSpace(c.Username)
	c.Bulk.Backpressure = strings.ToLower(strings.TrimSpace(c.Bulk.Backpressure))

	if c.Timeout <= 0 {
		return normalizedConfig{}, errors.New("elasticsearch: timeout must be positive")
	}
	if c.CloudID != "" && len(c.Addresses) > 0 {
		return normalizedConfig{}, errors.New("elasticsearch: addresses and cloud_id are mutually exclusive")
	}
	if c.CloudID == "" && len(c.Addresses) == 0 {
		return normalizedConfig{}, errors.New("elasticsearch: at least one address or cloud_id is required")
	}
	c.Addresses = append([]string(nil), c.Addresses...)
	seen := make(map[string]struct{}, len(c.Addresses))
	for i, raw := range c.Addresses {
		address := strings.TrimSpace(raw)
		parsed, err := url.Parse(address)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return normalizedConfig{}, fmt.Errorf("elasticsearch: address %d must be an http(s) URL without credentials, query, or fragment", i)
		}
		address = strings.TrimRight(address, "/")
		if _, exists := seen[address]; exists {
			return normalizedConfig{}, fmt.Errorf("elasticsearch: address %q is duplicated", address)
		}
		seen[address] = struct{}{}
		c.Addresses[i] = address
	}
	basicConfigured := c.Username != "" || c.Password != ""
	if basicConfigured && (c.Username == "" || c.Password == "") {
		return normalizedConfig{}, errors.New("elasticsearch: username and password must be configured together")
	}
	if c.APIKey != "" && basicConfigured {
		return normalizedConfig{}, errors.New("elasticsearch: api_key and basic authentication are mutually exclusive")
	}
	if c.Bulk.QueueCapacity <= 0 || c.Bulk.FlushActions <= 0 || c.Bulk.FlushBytes <= 0 || c.Bulk.FlushInterval <= 0 {
		return normalizedConfig{}, errors.New("elasticsearch: bulk queue and flush limits must be positive")
	}
	switch c.Bulk.Backpressure {
	case BackpressureBlock, BackpressureReject:
	default:
		return normalizedConfig{}, fmt.Errorf("elasticsearch: unsupported bulk backpressure %q", c.Bulk.Backpressure)
	}
	return normalizedConfig{Config: c}, nil
}
