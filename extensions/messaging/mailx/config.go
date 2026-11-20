package mailx

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	gomail "github.com/wneessen/go-mail"
)

// TLS policy values select how the client negotiates STARTTLS on a plain SMTP
// connection.
const (
	// TLSPolicyMandatory requires STARTTLS. It is the default and matches the
	// library default, so an upgrade never silently weakens transport security.
	TLSPolicyMandatory = "mandatory"
	// TLSPolicyOpportunistic uses STARTTLS when the server offers it.
	TLSPolicyOpportunistic = "opportunistic"
	// TLSPolicyNone disables STARTTLS. It exists for local test servers; a
	// production deployment must not use it.
	TLSPolicyNone = "none"
)

// Config configures one SMTP client. XBC binds it from one named section below
// plugins.mail (for example, plugins.mail.default).
//
// Port defaults to 25, the library default. Timeout and ConnectionTimeout are
// Go duration strings ("30s"), not seconds; their zero values leave the
// library defaults in place. Verify controls the construction-time
// connectivity check and defaults to true; disabling it makes construction
// lazy and should be reserved for deployments that deliberately tolerate an
// unavailable mail server at startup.
//
// Password is a secret. It is tagged mask:"true" so XBC's configuration view
// redacts it, and this package never renders it in errors, logs, or formatted
// output.
type Config struct {
	Host     string `yaml:"host" validate:"required"`
	Port     int    `yaml:"port" default:"25" validate:"min=1,max=65535"`
	Username string `yaml:"username"`
	Password string `yaml:"password" mask:"true"`

	// AuthType is an SMTP authentication mechanism understood by go-mail
	// ("plain", "login", "cram-md5", "scram-sha-256", "none", ...). An empty
	// value means autodiscover, which is also the legacy default.
	AuthType string `yaml:"auth_type"`

	SSL SSLConfig `yaml:"ssl"`
	TLS TLSConfig `yaml:"tls"`

	// Timeout bounds one SMTP conversation: dial, EHLO, TLS, AUTH, and transfer.
	// The zero value keeps the library default.
	Timeout time.Duration `yaml:"timeout" validate:"min=0"`
	// ConnectionTimeout bounds only the construction-time verification dial.
	// The zero value relies on Timeout, or on the library default, instead.
	ConnectionTimeout time.Duration `yaml:"connection_timeout" validate:"min=0"`

	Verify bool `yaml:"verify" default:"true"`
}

// SSLConfig selects implicit TLS, where the connection is encrypted from the
// first byte (typically port 465). It preserves the legacy ssl.enable and
// ssl.port semantics: Port overrides Config.Port when both are set, and
// leaving both unset connects to port 25 over implicit TLS, exactly as the
// legacy implementation did -- deployments should set port 465 explicitly.
type SSLConfig struct {
	Enable bool `yaml:"enable" default:"false"`
	Port   int  `yaml:"port" validate:"omitempty,min=1,max=65535"`
}

// TLSConfig selects the STARTTLS policy for a plain SMTP connection.
type TLSConfig struct {
	Policy string `yaml:"policy" default:"mandatory" validate:"oneof=mandatory opportunistic none"`
}

// String renders the configuration with its secret redacted. fmt uses it for
// %v and %+v; GoString covers %#v.
func (c Config) String() string {
	password := ""
	if c.Password != "" {
		password = "<redacted>"
	}
	return fmt.Sprintf(
		"mail{host:%s port:%d username:%q password:%s auth_type:%s ssl.enabled:%t ssl.port:%d tls.policy:%s timeout:%s connection_timeout:%s verify:%t}",
		c.Host, c.Port, c.Username, password, c.AuthType, c.SSL.Enable, c.SSL.Port, c.TLS.Policy, c.Timeout, c.ConnectionTimeout, c.Verify)
}

// GoString renders the configuration for %#v with its secret redacted.
func (c Config) GoString() string { return c.String() }

// prepareConfig implements plugin.ConfigSpec.Prepare. It enforces the
// invariants that per-field validate tags cannot express and canonicalizes the
// authentication type so construction cannot observe an unvalidated value. It
// performs no I/O.
func prepareConfig(config Config) (Config, error) {
	config.Host = strings.TrimSpace(config.Host)
	if config.Host == "" {
		return Config{}, errors.New("mail: host is required")
	}
	if config.Port < 1 || config.Port > 65535 {
		return Config{}, fmt.Errorf("mail: port %d must be between 1 and 65535", config.Port)
	}
	if config.SSL.Port != 0 {
		if !config.SSL.Enable {
			return Config{}, errors.New("mail: ssl.port requires ssl.enable")
		}
		if config.SSL.Port < 1 || config.SSL.Port > 65535 {
			return Config{}, fmt.Errorf("mail: ssl.port %d must be between 1 and 65535", config.SSL.Port)
		}
	}
	if config.TLS.Policy == "" {
		config.TLS.Policy = TLSPolicyMandatory
	}
	switch config.TLS.Policy {
	case TLSPolicyMandatory, TLSPolicyOpportunistic, TLSPolicyNone:
	default:
		return Config{}, fmt.Errorf("mail: tls.policy %q is not supported, want mandatory, opportunistic, or none", config.TLS.Policy)
	}
	if config.SSL.Enable && config.TLS.Policy == TLSPolicyNone {
		return Config{}, errors.New("mail: ssl.enable and tls.policy none are mutually exclusive; implicit TLS cannot be combined with disabled TLS")
	}
	if config.Timeout < 0 {
		return Config{}, fmt.Errorf("mail: timeout %s must not be negative", config.Timeout)
	}
	if config.ConnectionTimeout < 0 {
		return Config{}, fmt.Errorf("mail: connection_timeout %s must not be negative", config.ConnectionTimeout)
	}
	authType, err := config.smtpAuthType()
	if err != nil {
		return Config{}, err
	}
	config.AuthType = string(authType)
	return config, nil
}

// smtpAuthType resolves the configured authentication type. An empty value
// means autodiscover, exactly as the legacy client defaulted.
func (c Config) smtpAuthType() (gomail.SMTPAuthType, error) {
	value := strings.TrimSpace(c.AuthType)
	if value == "" {
		return gomail.SMTPAuthAutoDiscover, nil
	}
	var authType gomail.SMTPAuthType
	if err := authType.UnmarshalString(value); err != nil {
		return "", fmt.Errorf("mail: auth_type %q: %w", value, err)
	}
	if authType == gomail.SMTPAuthCustom {
		return "", errors.New(`mail: auth_type "custom" requires an application-supplied mechanism, which this plugin does not accept; choose a named mechanism or leave the field empty for autodiscover`)
	}
	return authType, nil
}

// address is the host:port the instance connects to and the only addressing
// detail this package reports in errors or probes. It never includes
// credentials.
func (c Config) address() string {
	port := c.Port
	if c.SSL.Enable && c.SSL.Port != 0 {
		port = c.SSL.Port
	}
	if port == 0 {
		port = gomail.DefaultPort
	}
	return net.JoinHostPort(c.Host, strconv.Itoa(port))
}

// tlsPolicy maps the configured policy onto the library's. The zero value is
// mandatory so a directly constructed Config cannot silently downgrade.
func (c Config) tlsPolicy() gomail.TLSPolicy {
	switch c.TLS.Policy {
	case TLSPolicyOpportunistic:
		return gomail.TLSOpportunistic
	case TLSPolicyNone:
		return gomail.NoTLS
	default:
		return gomail.TLSMandatory
	}
}

// clientOptions translates the prepared configuration into library options.
// The TLS policy is always set explicitly so the secure default does not
// depend on a library default that could change.
func (c Config) clientOptions() ([]gomail.Option, error) {
	options := make([]gomail.Option, 0, 8)
	// Port is applied before the SSL port because the SSL port overrides it,
	// preserving the legacy precedence.
	if c.Port > 0 {
		options = append(options, gomail.WithPort(c.Port))
	}
	if c.SSL.Enable {
		options = append(options, gomail.WithSSL())
		if c.SSL.Port > 0 {
			options = append(options, gomail.WithPort(c.SSL.Port))
		}
	}
	if c.Username != "" {
		options = append(options, gomail.WithUsername(c.Username))
	}
	if c.Password != "" {
		options = append(options, gomail.WithPassword(c.Password))
	}
	authType, err := c.smtpAuthType()
	if err != nil {
		return nil, err
	}
	options = append(options, gomail.WithSMTPAuth(authType))
	if c.Timeout > 0 {
		options = append(options, gomail.WithTimeout(c.Timeout))
	}
	options = append(options, gomail.WithTLSPolicy(c.tlsPolicy()))
	return options, nil
}
