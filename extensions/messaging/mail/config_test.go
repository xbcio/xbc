package mail

import (
	"fmt"
	"strings"
	"testing"
	"time"

	gomail "github.com/wneessen/go-mail"

	xbcconfig "github.com/xbcio/xbc/config"
)

func TestConfigDefaults(t *testing.T) {
	config := bindConfig(t, map[string]any{"host": "smtp.example.com"})

	if config.Host != "smtp.example.com" {
		t.Fatalf("Host = %q, want the configured host", config.Host)
	}
	if config.Port != 25 {
		t.Fatalf("Port = %d, want the library default 25", config.Port)
	}
	if config.Username != "" || config.Password != "" {
		t.Fatalf("unexpected credential defaults: %+v", config)
	}
	if config.AuthType != string(gomail.SMTPAuthAutoDiscover) {
		t.Fatalf("AuthType = %q, want autodiscover", config.AuthType)
	}
	if config.SSL.Enable || config.SSL.Port != 0 {
		t.Fatalf("unexpected SSL defaults: %+v", config.SSL)
	}
	if config.TLS.Policy != TLSPolicyMandatory {
		t.Fatalf("TLS.Policy = %q, want %q", config.TLS.Policy, TLSPolicyMandatory)
	}
	if config.Timeout != 0 || config.ConnectionTimeout != 0 {
		t.Fatalf("unexpected timeout defaults: %+v", config)
	}
	if !config.Verify {
		t.Fatal("Verify = false, want true by default")
	}
}

func TestConfigBindsAllSupportedOptions(t *testing.T) {
	config := bindConfig(t, map[string]any{
		"host":               "  smtp.internal  ",
		"port":               2525,
		"username":           "service",
		"password":           "secret",
		"auth_type":          "plain",
		"ssl":                map[string]any{"enable": true, "port": 2465},
		"tls":                map[string]any{"policy": "opportunistic"},
		"timeout":            "30s",
		"connection_timeout": "5s",
		"verify":             false,
	})

	if config.Host != "smtp.internal" {
		t.Fatalf("Host = %q, want the trimmed host", config.Host)
	}
	if config.Port != 2525 {
		t.Fatalf("Port = %d, want 2525", config.Port)
	}
	if config.Username != "service" || config.Password != "secret" {
		t.Fatalf("credentials = %q/%q, want service/secret", config.Username, config.Password)
	}
	if config.AuthType != string(gomail.SMTPAuthPlain) {
		t.Fatalf("AuthType = %q, want PLAIN", config.AuthType)
	}
	if !config.SSL.Enable || config.SSL.Port != 2465 {
		t.Fatalf("SSL = %+v, want enabled on port 2465", config.SSL)
	}
	if config.TLS.Policy != TLSPolicyOpportunistic {
		t.Fatalf("TLS.Policy = %q, want opportunistic", config.TLS.Policy)
	}
	if config.Timeout != 30*time.Second || config.ConnectionTimeout != 5*time.Second {
		t.Fatalf("timeouts = %s/%s, want 30s/5s", config.Timeout, config.ConnectionTimeout)
	}
	if config.Verify {
		t.Fatal("explicit verify=false was overwritten by its default")
	}
	if got, want := config.address(), "smtp.internal:2465"; got != want {
		t.Fatalf("address() = %q, want %q (ssl.port overrides port)", got, want)
	}
}

func TestPrepareConfigCanonicalizesAuthType(t *testing.T) {
	base := bindConfig(t, map[string]any{"host": "smtp.example.com"})
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty means autodiscover", input: "", want: string(gomail.SMTPAuthAutoDiscover)},
		{name: "auto alias", input: "auto", want: string(gomail.SMTPAuthAutoDiscover)},
		{name: "plain", input: "plain", want: string(gomail.SMTPAuthPlain)},
		{name: "login", input: "login", want: string(gomail.SMTPAuthLogin)},
		{name: "no auth", input: "none", want: string(gomail.SMTPAuthNoAuth)},
		{name: "scram", input: "scram-sha-256", want: string(gomail.SMTPAuthSCRAMSHA256)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := base
			config.AuthType = test.input
			prepared, err := prepareConfig(config)
			if err != nil {
				t.Fatalf("prepareConfig() error = %v", err)
			}
			if prepared.AuthType != test.want {
				t.Fatalf("AuthType = %q, want %q", prepared.AuthType, test.want)
			}
		})
	}
}

func TestPrepareConfigRejectsInvalidValues(t *testing.T) {
	base := bindConfig(t, map[string]any{"host": "smtp.example.com"})
	tests := []struct {
		name   string
		edit   func(*Config)
		expect string
	}{
		{name: "empty host", edit: func(c *Config) { c.Host = "   " }, expect: "host is required"},
		{name: "port too low", edit: func(c *Config) { c.Port = 0 }, expect: "port"},
		{name: "port too high", edit: func(c *Config) { c.Port = 70000 }, expect: "port"},
		{
			name:   "ssl.port without ssl.enable",
			edit:   func(c *Config) { c.SSL = SSLConfig{Port: 465} },
			expect: "ssl.port requires ssl.enable",
		},
		{
			name:   "ssl.port out of range",
			edit:   func(c *Config) { c.SSL = SSLConfig{Enable: true, Port: 70000} },
			expect: "ssl.port",
		},
		{
			name:   "unknown tls policy",
			edit:   func(c *Config) { c.TLS.Policy = "plaintext" },
			expect: "tls.policy",
		},
		{
			name:   "implicit tls with disabled starttls",
			edit:   func(c *Config) { c.SSL = SSLConfig{Enable: true, Port: 465}; c.TLS.Policy = TLSPolicyNone },
			expect: "mutually exclusive",
		},
		{
			name:   "negative timeout",
			edit:   func(c *Config) { c.Timeout = -time.Second },
			expect: "timeout",
		},
		{
			name:   "negative connection timeout",
			edit:   func(c *Config) { c.ConnectionTimeout = -time.Second },
			expect: "connection_timeout",
		},
		{
			name:   "custom auth needs an application mechanism",
			edit:   func(c *Config) { c.AuthType = "custom" },
			expect: "application-supplied mechanism",
		},
		{
			name:   "unsupported auth",
			edit:   func(c *Config) { c.AuthType = "carrier-pigeon" },
			expect: "auth_type",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := base
			test.edit(&config)
			_, err := prepareConfig(config)
			if err == nil {
				t.Fatal("prepareConfig() error = nil")
			}
			if !strings.Contains(err.Error(), test.expect) {
				t.Fatalf("error = %q, want it to contain %q", err, test.expect)
			}
		})
	}
}

func TestConfigValidationRejectsNegativeDurations(t *testing.T) {
	for name, values := range map[string]map[string]any{
		"timeout":            {"host": "smtp.example.com", "timeout": "-1s"},
		"connection_timeout": {"host": "smtp.example.com", "connection_timeout": "-1s"},
	} {
		t.Run(name, func(t *testing.T) {
			config, err := bindConfigWithoutValidation(values)
			if err != nil {
				t.Fatalf("Bind() error = %v", err)
			}
			if err := xbcconfig.Validate(&config, "plugins.mail.default"); err == nil {
				t.Fatal("Validate() error = nil, want a negative duration rejected")
			}
		})
	}
}

func TestConfigRejectsUnknownFields(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]any
	}{
		{
			name:   "top level",
			values: map[string]any{"host": "smtp.example.com", "smtp_auth_type": "plain"},
		},
		{
			name:   "nested",
			values: map[string]any{"host": "smtp.example.com", "ssl": map[string]any{"enable": false, "starttls": true}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := bindConfigWithoutValidation(test.values); err == nil {
				t.Fatal("Bind() error = nil, want an unknown field rejected")
			} else if !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("error = %q, want an unknown-field error", err)
			}
		})
	}
}

// TestSecretNeverRendered is the load-bearing guard for the password: the
// value is bound and available to the client, yet neither an error nor any
// formatted form of the configuration may contain it.
func TestSecretNeverRendered(t *testing.T) {
	const secret = "sup3r-secret-password-value"
	values := map[string]any{
		"host":     "smtp.example.com",
		"username": "service",
		"password": secret,
	}
	config := bindConfig(t, values)
	if config.Password != secret {
		t.Fatal("the test secret did not reach the configuration; the assertions below would be vacuous")
	}

	for _, format := range []string{"%v", "%+v", "%#v"} {
		if rendered := fmt.Sprintf(format, config); strings.Contains(rendered, secret) {
			t.Fatalf("%s rendered the password: %q", format, rendered)
		}
		if rendered := fmt.Sprintf(format, &config); strings.Contains(rendered, secret) {
			t.Fatalf("%s rendered the password through a pointer: %q", format, rendered)
		}
	}
	if rendered := config.String(); strings.Contains(rendered, secret) {
		t.Fatalf("String() rendered the password: %q", rendered)
	}
	if rendered := config.GoString(); strings.Contains(rendered, secret) {
		t.Fatalf("GoString() rendered the password: %q", rendered)
	}
	if !strings.Contains(config.String(), "<redacted>") {
		t.Fatalf("String() = %q, want the password marked as redacted", config.String())
	}

	for name, edit := range map[string]map[string]any{
		"invalid auth_type":   {"host": "smtp.example.com", "password": secret, "auth_type": "carrier-pigeon"},
		"invalid tls.policy":  {"host": "smtp.example.com", "password": secret, "tls": map[string]any{"policy": "plaintext"}},
		"negative timeout":    {"host": "smtp.example.com", "password": secret, "timeout": "-1s"},
		"ssl.port mismatched": {"host": "smtp.example.com", "password": secret, "ssl": map[string]any{"port": 465}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := bindPreparedConfig(edit)
			if err == nil {
				t.Fatal("bindPreparedConfig() error = nil")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error rendered the password: %q", err)
			}
		})
	}
}

func TestConfigAddressSelection(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]any
		want   string
	}{
		{name: "default port", values: map[string]any{"host": "smtp.example.com"}, want: "smtp.example.com:25"},
		{name: "explicit port", values: map[string]any{"host": "smtp.example.com", "port": 587}, want: "smtp.example.com:587"},
		{
			name:   "ssl port overrides",
			values: map[string]any{"host": "smtp.example.com", "port": 587, "ssl": map[string]any{"enable": true, "port": 465}},
			want:   "smtp.example.com:465",
		},
		{
			name:   "ssl without a port keeps the configured port",
			values: map[string]any{"host": "smtp.example.com", "ssl": map[string]any{"enable": true}},
			want:   "smtp.example.com:25",
		},
		{name: "ipv6 host", values: map[string]any{"host": "::1", "port": 2525}, want: "[::1]:2525"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := bindConfig(t, test.values)
			if got := config.address(); got != test.want {
				t.Fatalf("address() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestTLSPolicyMapping(t *testing.T) {
	tests := map[string]gomail.TLSPolicy{
		TLSPolicyMandatory:     gomail.TLSMandatory,
		TLSPolicyOpportunistic: gomail.TLSOpportunistic,
		TLSPolicyNone:          gomail.NoTLS,
	}
	for configured, want := range tests {
		config := Config{TLS: TLSConfig{Policy: configured}}
		if got := config.tlsPolicy(); got != want {
			t.Fatalf("tlsPolicy(%q) = %v, want %v", configured, got, want)
		}
	}
	if got := (Config{}).tlsPolicy(); got != gomail.TLSMandatory {
		t.Fatalf("zero-value policy = %v, want the secure default %v", got, gomail.TLSMandatory)
	}
}
