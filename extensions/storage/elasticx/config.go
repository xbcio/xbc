package elasticx

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

// ProbePolicy values decide what a failed Init-time health probe means.
const (
	// ProbePolicyFail aborts construction. It is the default because a
	// cluster the instance cannot reach at startup is usually a configuration
	// or network error worth surfacing before the process serves traffic.
	ProbePolicyFail = "fail"
	// ProbePolicyWarn records the failure and lets construction continue. Use
	// it for a cluster whose unavailability must not stop the process.
	ProbePolicyWarn = "warn"
)

// Config configures one named Elasticsearch cluster. Secrets are accepted as
// configuration values for integration with XBC secret sources; this package
// never includes them in errors or logs.
type Config struct {
	Addresses []string `yaml:"addresses"`
	CloudID   string   `yaml:"cloud_id"`
	APIKey    string   `yaml:"api_key" mask:"true"`
	Username  string   `yaml:"username"`
	Password  string   `yaml:"password" mask:"true"`

	// AllowEmptyPassword permits username without password. The pair is
	// required together by default because a password-only section cannot
	// authenticate, and a username-only section is far more likely to be a
	// secret-injection failure than an intentional deployment shape. The
	// explicit switch covers clusters that need no authentication but whose
	// shared configuration still carries a service username, which is the
	// shape SAS deploys today. Note that go-elasticsearch sends Basic
	// authentication only when both fields are non-empty, so this switch
	// accepts the username without turning it into a credential; requests are
	// unauthenticated, exactly as they are for SAS's current client.
	AllowEmptyPassword bool `yaml:"allow_empty_password" default:"false"`

	// CompressRequestBody gzips request bodies. It is off by default because
	// it trades client CPU for bandwidth, and a proxy in front of the cluster
	// may reject a compressed body.
	CompressRequestBody bool `yaml:"compress_request_body" default:"false"`

	// DisableProxy stops the package-built HTTP transport from consulting the
	// process proxy environment (HTTP_PROXY, HTTPS_PROXY, NO_PROXY). It exists
	// for an internal cluster that must be reached directly, where a proxy
	// configured for outbound traffic would otherwise intercept the
	// connection. It has no effect on a caller-supplied transport; New rejects
	// the combination instead of silently ignoring the setting.
	DisableProxy bool `yaml:"disable_proxy" default:"false"`

	TLS         TLSConfig     `yaml:"tls"`
	Timeout     time.Duration `yaml:"timeout" default:"10s" validate:"gt=0"`
	HealthProbe bool          `yaml:"health_probe" default:"true"`

	// ProbePolicy decides what a failed startup health probe means: "fail"
	// (the default) aborts construction, "warn" logs the failure and lets the
	// instance start. It only affects the startup probe; health_probe: false
	// skips that probe entirely.
	ProbePolicy string     `yaml:"probe_policy" default:"fail" validate:"oneof=fail warn"`
	Bulk        BulkConfig `yaml:"bulk"`
}

// TLSConfig optionally supplies PEM certificate authorities for HTTPS nodes
// and the one explicit escape hatch for a certificate that cannot be verified.
type TLSConfig struct {
	CAFile string `yaml:"ca_file"`

	// InsecureSkipVerify disables certificate verification. It exists for
	// internal clusters with a self-signed certificate and must be an explicit
	// opt-in: it turns TLS into encryption without authentication, which is
	// indistinguishable from cleartext against an active attacker. Enabling it
	// makes the client build its own http.Transport from
	// http.DefaultTransport; ca_file, when also set, is installed as its root
	// pool even though verification is disabled.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify" default:"false"`
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
		ProbePolicy: ProbePolicyFail,
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
	if strings.TrimSpace(c.ProbePolicy) == "" {
		c.ProbePolicy = d.ProbePolicy
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
	c.ProbePolicy = strings.ToLower(strings.TrimSpace(c.ProbePolicy))

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
	if c.Password != "" && c.Username == "" {
		return normalizedConfig{}, errors.New("elasticsearch: username and password must be configured together")
	}
	if c.Username != "" && c.Password == "" && !c.AllowEmptyPassword {
		return normalizedConfig{}, errors.New("elasticsearch: username and password must be configured together; set allow_empty_password to accept a username with an empty password")
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
	switch c.ProbePolicy {
	case ProbePolicyFail, ProbePolicyWarn:
	default:
		return normalizedConfig{}, fmt.Errorf("elasticsearch: unsupported probe policy %q", c.ProbePolicy)
	}
	return normalizedConfig{Config: c}, nil
}
