package httpclient

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// Config is the configuration of one named outbound HTTP client. Every
// downstream gets its own instance -- plugins.http-client.<name> -- so its
// timeout, retry policy, TLS material and connection pool are independent of
// every other downstream's, and a failure or a slow peer is contained to the
// instance configured for it.
//
// The sub-sections are value types rather than pointers. The binder fills
// defaults and decodes sections through the addressable struct, so a pointer
// sub-structure would either stay nil when nothing configures it or need its
// own allocation dance; value sections keep "unset" spelled as the zero value
// and defaulting spelled in one place (normalized, plus the tag defaults the
// binder applies).
type Config struct {
	// BaseURL names the endpoint this instance calls. It is optional and it is
	// configuration for the operator, not a request rewrite: the plugin hands
	// out a standard *http.Client, and a caller that configures its request
	// library with the same value (resty's SetBaseURL, for example) decides
	// what goes on the wire. The plugin validates it -- absolute http or https
	// URL, no userinfo -- and reports its scheme://host in the validate and
	// startup report.
	BaseURL string `yaml:"base_url"`

	// Timeout bounds one attempt, response body included -- the span
	// http.Client.Timeout covers, applied per attempt rather than per call.
	Timeout time.Duration `yaml:"timeout" default:"10s"`

	// TotalTimeout bounds everything one call may consume: every attempt,
	// every wait between attempts and the final response read. It defaults to
	// Timeout, so the default configuration makes one bounded attempt. A
	// caller context with an earlier deadline wins over it.
	TotalTimeout time.Duration `yaml:"total_timeout"`

	// Retry controls which failures are retried and how the waits between
	// attempts are computed.
	Retry RetryConfig `yaml:"retry"`

	// TLS verifies the peer and, when configured, presents this process's
	// certificate.
	TLS TLSConfig `yaml:"tls"`

	// MaxIdleConnsPerHost bounds the idle connection pool kept per host. The
	// default matches net/http's own, so an instance that names no pool
	// parameters behaves like a plainly constructed http.Client.
	MaxIdleConnsPerHost int `yaml:"max_idle_conns_per_host" default:"2"`

	// IdleConnTimeout is how long an idle pooled connection is kept before it
	// is closed. The default matches net/http's own.
	IdleConnTimeout time.Duration `yaml:"idle_conn_timeout" default:"90s"`
}

// RetryConfig is the retry half of one instance's configuration. Its defaults
// mean "no retries": a deployment turns retrying on deliberately, per
// downstream, because which failures may be retried is a property of that
// downstream's contract rather than a global nicety.
type RetryConfig struct {
	// MaxAttempts is the total number of attempts including the first, so the
	// default 1 disables retrying outright.
	MaxAttempts int `yaml:"max_attempts" default:"1"`

	// InitialBackoff is the base wait after the first retryable outcome. Each
	// further retry doubles it, capped at MaxBackoff. The wait actually slept
	// is jittered into the upper half of the computed backoff, so independent
	// clients retrying against one failing peer do not synchronize into a
	// thundering herd.
	InitialBackoff time.Duration `yaml:"initial_backoff" default:"500ms"`

	// MaxBackoff caps the doubled backoff.
	MaxBackoff time.Duration `yaml:"max_backoff" default:"30s"`

	// MaxRetryAfter caps how long a Retry-After header on a 429 is honoured.
	// Its default is 30s; an explicit zero means the retry follows
	// immediately, which is the honest reading of "cap at zero" and the
	// behaviour a caller wants when it has sized TotalTimeout itself.
	MaxRetryAfter time.Duration `yaml:"max_retry_after" default:"30s"`
}

// TLSConfig is the client half of the TLS section. It verifies the peer
// against the system roots or a private CA and optionally presents a client
// certificate pair.
//
// There is deliberately no insecure_skip_verify: a process that dials a
// downstream over TLS either verifies it or should not be speaking TLS to it.
// The migration that motivated this module had a global "skip verification"
// switch in its previous HTTP client, and reproducing that switch here would
// reproduce the vulnerability the migration is meant to retire.
type TLSConfig struct {
	// CAFile is the PEM bundle the peer is verified against. Empty uses the
	// system roots.
	CAFile string `yaml:"ca_file"`

	// CertFile and KeyFile are the client certificate pair presented when the
	// peer asks for one (mTLS). They must be configured together.
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`

	// ServerName overrides the name verified against the peer certificate's
	// SANs, for the deployment whose dialed address is not the certificate's.
	ServerName string `yaml:"server_name"`

	// MinVersion is the lowest TLS version accepted, spelled "1.2" or "1.3".
	// TLS 1.0 and 1.1 are deliberately not addressable, the same stance the
	// serving side takes.
	MinVersion string `yaml:"min_version" default:"1.2" validate:"omitempty,oneof=1.2 1.3"`
}

const defaultTLSMinVersion = "1.2"

// DefaultConfig returns the same defaults the configuration binder applies.
func DefaultConfig() Config {
	return Config{
		Timeout:             10 * time.Second,
		Retry:               defaultRetryConfig(),
		TLS:                 defaultTLSConfig(),
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
	}
}

func defaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts:    1,
		InitialBackoff: 500 * time.Millisecond,
		MaxBackoff:     30 * time.Second,
		MaxRetryAfter:  30 * time.Second,
	}
}

func defaultTLSConfig() TLSConfig {
	return TLSConfig{MinVersion: defaultTLSMinVersion}
}

// Validate checks defaults and the constraints struct tags cannot express,
// without any network I/O. It reports on a normalized copy, so a directly
// constructed Config validates the same way a bound one does.
func (c Config) Validate() error {
	_, err := c.normalized()
	return err
}

// normalized applies defaults and validates. It is the ConfigSpec.Prepare
// half, so a configuration that reaches a factory is always complete and
// checked whether it was bound from a file or constructed directly.
func (c Config) normalized() (Config, error) {
	d := DefaultConfig()
	if c.Timeout == 0 {
		c.Timeout = d.Timeout
	}
	if c.MaxIdleConnsPerHost == 0 {
		c.MaxIdleConnsPerHost = d.MaxIdleConnsPerHost
	}
	if c.IdleConnTimeout == 0 {
		c.IdleConnTimeout = d.IdleConnTimeout
	}
	if c.TotalTimeout == 0 {
		c.TotalTimeout = c.Timeout
	}
	applyRetryDefaults(&c.Retry)
	applyTLSDefaults(&c.TLS)

	if strings.TrimSpace(c.BaseURL) != "" {
		trimmed := strings.TrimSpace(c.BaseURL)
		if trimmed != c.BaseURL || strings.ContainsFunc(trimmed, unicode.IsSpace) {
			return Config{}, errors.New("http-client: base_url must not contain whitespace")
		}
		parsed, err := url.Parse(trimmed)
		if err != nil {
			// The parse error quotes the URL it rejected, and a rejected URL
			// is exactly where credentials live, so it is not wrapped: the
			// message states the rule instead of the value.
			return Config{}, errors.New("http-client: base_url must be an absolute http or https URL")
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return Config{}, fmt.Errorf("http-client: base_url scheme %q is not supported; use http or https", parsed.Scheme)
		}
		if parsed.Host == "" {
			return Config{}, errors.New("http-client: base_url must name a host")
		}
		if parsed.User != nil {
			// Rejected rather than stripped: a credential embedded in a URL
			// leaks into every place a URL is rendered -- errors, redirects,
			// access logs -- and the caller that needs one already sets it as
			// a header, where the identity model of the peer expects it.
			return Config{}, errors.New("http-client: base_url must not embed credentials (user:pass@); set the credential as a request header instead")
		}
		c.BaseURL = trimmed
	}
	if c.Timeout <= 0 {
		return Config{}, errors.New("http-client: timeout must be greater than zero")
	}
	if c.TotalTimeout < 0 {
		return Config{}, errors.New("http-client: total_timeout must not be negative")
	}
	if c.MaxIdleConnsPerHost < 0 {
		return Config{}, errors.New("http-client: max_idle_conns_per_host must not be negative")
	}
	if c.IdleConnTimeout <= 0 {
		return Config{}, errors.New("http-client: idle_conn_timeout must be greater than zero")
	}
	if err := c.Retry.validate(); err != nil {
		return Config{}, err
	}
	if err := c.TLS.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func applyRetryDefaults(r *RetryConfig) {
	d := defaultRetryConfig()
	if r.MaxAttempts == 0 {
		r.MaxAttempts = d.MaxAttempts
	}
	if r.InitialBackoff == 0 {
		r.InitialBackoff = d.InitialBackoff
	}
	if r.MaxBackoff == 0 {
		r.MaxBackoff = d.MaxBackoff
	}
	if r.MaxRetryAfter == 0 {
		r.MaxRetryAfter = d.MaxRetryAfter
	}
	// MaxRetryAfter keeps its explicit zero only through a direct
	// construction; the binder's default tag and DefaultConfig both supply
	// 30s, so "unset" and "explicit zero" cannot be told apart here and the
	// documented default wins.
}

func (r RetryConfig) validate() error {
	if r.MaxAttempts < 1 {
		return errors.New("http-client: retry.max_attempts must be at least 1")
	}
	if r.InitialBackoff <= 0 {
		return errors.New("http-client: retry.initial_backoff must be greater than zero")
	}
	if r.MaxBackoff < r.InitialBackoff {
		return errors.New("http-client: retry.max_backoff must not be less than retry.initial_backoff")
	}
	if r.MaxRetryAfter < 0 {
		return errors.New("http-client: retry.max_retry_after must not be negative")
	}
	return nil
}

func applyTLSDefaults(t *TLSConfig) {
	if t.MinVersion == "" {
		t.MinVersion = defaultTLSMinVersion
	}
}

func (t TLSConfig) validate() error {
	certFile := strings.TrimSpace(t.CertFile)
	keyFile := strings.TrimSpace(t.KeyFile)
	if (certFile == "") != (keyFile == "") {
		return errors.New("http-client: tls.cert_file and tls.key_file must be set together")
	}
	if t.CertFile != certFile || t.KeyFile != keyFile {
		return errors.New("http-client: tls.cert_file and tls.key_file must not be padded with whitespace")
	}
	if t.CAFile != strings.TrimSpace(t.CAFile) || t.ServerName != strings.TrimSpace(t.ServerName) {
		return errors.New("http-client: tls.ca_file and tls.server_name must not be padded with whitespace")
	}
	if _, err := t.minVersion(); err != nil {
		return err
	}
	return nil
}

// endpoint renders the configured base_url as the scheme://host an operator
// report may show: userinfo is rejected at validation, and path and query are
// deliberately dropped because they routinely carry credentials and ids that
// belong in neither a report nor a log. An unset base_url renders empty and
// the caller omits the line.
func (c Config) endpoint() string {
	if c.BaseURL == "" {
		return ""
	}
	parsed, err := url.Parse(c.BaseURL)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}
