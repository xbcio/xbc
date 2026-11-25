package mailx

import "time"

// Option customizes a Config constructed directly with New or NewFromOptions.
// Options are applied to a copy of the configuration in the order supplied,
// and a nil Option is ignored.
type Option func(*Config)

// WithHost sets the SMTP server host.
func WithHost(host string) Option {
	return func(config *Config) { config.Host = host }
}

// WithPort sets the SMTP server port. When implicit TLS is enabled with its own
// port, that port takes precedence, exactly as it does for a bound section.
func WithPort(port int) Option {
	return func(config *Config) { config.Port = port }
}

// WithCredentials sets the SMTP username and password. The password stays a
// secret: this package never renders it in errors, logs, or formatted output.
func WithCredentials(username, password string) Option {
	return func(config *Config) {
		config.Username = username
		config.Password = password
	}
}

// WithAuthType sets the SMTP authentication mechanism. An empty value means
// autodiscover, matching a bound section.
func WithAuthType(authType string) Option {
	return func(config *Config) { config.AuthType = authType }
}

// WithSSL replaces the implicit TLS settings. Enabling SSL with a Port makes
// that port override the configured address port.
func WithSSL(ssl SSLConfig) Option {
	return func(config *Config) { config.SSL = ssl }
}

// WithTLSPolicy sets the STARTTLS policy: mandatory, opportunistic, or none.
func WithTLSPolicy(policy string) Option {
	return func(config *Config) { config.TLS.Policy = policy }
}

// WithTimeout bounds one SMTP conversation: dial, EHLO, TLS, AUTH, and
// transfer. The zero value keeps the library default.
func WithTimeout(timeout time.Duration) Option {
	return func(config *Config) { config.Timeout = timeout }
}

// WithConnectionTimeout bounds the construction-time verification dial. The
// zero value relies on Timeout, or on the library default, instead.
func WithConnectionTimeout(timeout time.Duration) Option {
	return func(config *Config) { config.ConnectionTimeout = timeout }
}

// WithVerify controls the construction-time connectivity check. Verification
// is on in DefaultConfig and off in a zero Config.
func WithVerify(verify bool) Option {
	return func(config *Config) { config.Verify = verify }
}

// applyOptions returns cfg with opts applied in order. Config is a value, so
// the caller's copy is never mutated; nil options are ignored.
func applyOptions(cfg Config, opts ...Option) Config {
	for _, option := range opts {
		if option != nil {
			option(&cfg)
		}
	}
	return cfg
}
