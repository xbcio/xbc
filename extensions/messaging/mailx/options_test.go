package mailx

import (
	"strings"
	"testing"
	"time"
)

// TestOptionsSetTheirFields applies each option to DefaultConfig and pins the
// field it owns, so a broken option cannot hide behind another.
func TestOptionsSetTheirFields(t *testing.T) {
	tests := []struct {
		name   string
		option Option
		check  func(t *testing.T, config Config)
	}{
		{
			name:   "host",
			option: WithHost("smtp.example.com"),
			check: func(t *testing.T, config Config) {
				if config.Host != "smtp.example.com" {
					t.Fatalf("Host = %q, want smtp.example.com", config.Host)
				}
			},
		},
		{
			name:   "port",
			option: WithPort(2525),
			check: func(t *testing.T, config Config) {
				if config.Port != 2525 {
					t.Fatalf("Port = %d, want 2525", config.Port)
				}
			},
		},
		{
			name:   "credentials",
			option: WithCredentials("service", "secret"),
			check: func(t *testing.T, config Config) {
				if config.Username != "service" || config.Password != "secret" {
					t.Fatalf("credentials = %q/%q, want service/secret", config.Username, config.Password)
				}
			},
		},
		{
			name:   "auth type",
			option: WithAuthType("plain"),
			check: func(t *testing.T, config Config) {
				if config.AuthType != "plain" {
					t.Fatalf("AuthType = %q, want plain", config.AuthType)
				}
			},
		},
		{
			name:   "ssl",
			option: WithSSL(SSLConfig{Enable: true, Port: 465}),
			check: func(t *testing.T, config Config) {
				if !config.SSL.Enable || config.SSL.Port != 465 {
					t.Fatalf("SSL = %+v, want enabled on port 465", config.SSL)
				}
			},
		},
		{
			name:   "tls policy",
			option: WithTLSPolicy(TLSPolicyOpportunistic),
			check: func(t *testing.T, config Config) {
				if config.TLS.Policy != TLSPolicyOpportunistic {
					t.Fatalf("TLS.Policy = %q, want opportunistic", config.TLS.Policy)
				}
			},
		},
		{
			name:   "timeout",
			option: WithTimeout(30 * time.Second),
			check: func(t *testing.T, config Config) {
				if config.Timeout != 30*time.Second {
					t.Fatalf("Timeout = %s, want 30s", config.Timeout)
				}
			},
		},
		{
			name:   "connection timeout",
			option: WithConnectionTimeout(5 * time.Second),
			check: func(t *testing.T, config Config) {
				if config.ConnectionTimeout != 5*time.Second {
					t.Fatalf("ConnectionTimeout = %s, want 5s", config.ConnectionTimeout)
				}
			},
		},
		{
			name:   "verify",
			option: WithVerify(false),
			check: func(t *testing.T, config Config) {
				if config.Verify {
					t.Fatal("Verify = true, want the option to override the DefaultConfig default")
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.check(t, applyOptions(DefaultConfig(), test.option))
		})
	}
}

// TestApplyOptionsIgnoresNilAndCopies proves a nil Option is skipped rather
// than panicking, and the caller's Config is never mutated.
func TestApplyOptionsIgnoresNilAndCopies(t *testing.T) {
	original := DefaultConfig()
	config := applyOptions(original, nil, WithHost("smtp.example.com"), nil)
	if config.Host != "smtp.example.com" {
		t.Fatalf("Host = %q, want smtp.example.com", config.Host)
	}
	if original.Host != "" {
		t.Fatalf("applyOptions mutated the caller's Config: Host = %q", original.Host)
	}
}

// TestNewAppliesOptionsOverConfig proves New uses the option-adjusted copy of
// cfg: both the verification probe and the send reach the server named by the
// options, not the one named by cfg.
func TestNewAppliesOptionsOverConfig(t *testing.T) {
	configured := newFakeSMTPServer(t)
	overridden := newFakeSMTPServer(t)
	host, port := overridden.hostPort(t)

	config := bindConfig(t, serverValues(t, configured))
	sender, err := New(config, WithHost(host), WithPort(port), WithVerify(true))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	closer, ok := sender.(Closer)
	if !ok {
		t.Fatalf("New() returned %T, which does not implement Closer", sender)
	}
	t.Cleanup(func() { _ = closer.Close() })

	overridden.waitForAccepted(t, 1)
	if messages := configured.received(); len(messages) != 0 {
		t.Fatalf("the server named by cfg received %d messages, want none", len(messages))
	}
	sendMessage(t, sender, "override@example.com", "options override cfg")
	assertSingleMessage(t, overridden, "options override cfg")
}

// TestNewFromOptionsBuildsASender covers the defaults-plus-options path end to
// end against the fake server; nil options are ignored.
func TestNewFromOptionsBuildsASender(t *testing.T) {
	server := newFakeSMTPServer(t)
	host, port := server.hostPort(t)

	sender, err := NewFromOptions(
		nil,
		WithHost(host),
		WithPort(port),
		WithAuthType("none"),
		WithTLSPolicy(TLSPolicyNone),
		WithVerify(false),
		nil,
	)
	if err != nil {
		t.Fatalf("NewFromOptions() error = %v", err)
	}
	closer, ok := sender.(Closer)
	if !ok {
		t.Fatalf("NewFromOptions() returned %T, which does not implement Closer", sender)
	}
	t.Cleanup(func() { _ = closer.Close() })

	sendMessage(t, sender, "options@example.com", "options construction")
	assertSingleMessage(t, server, "options construction")
}

// TestNewFromOptionsRequiresHost proves the defaults path still enforces the
// host-required rule before any network activity.
func TestNewFromOptionsRequiresHost(t *testing.T) {
	sender, err := NewFromOptions(WithPort(2525), nil)
	if err == nil {
		t.Fatal("NewFromOptions() error = nil, want the missing host rejected")
	}
	if sender != nil {
		t.Fatalf("NewFromOptions() returned %T without a host", sender)
	}
	if !strings.Contains(err.Error(), "host is required") {
		t.Fatalf("error = %q, want the missing host named", err)
	}
}

// TestSSLPortOverridesPortThroughOptions pins the legacy precedence on both
// construction paths: ssl.port wins over port when implicit TLS is enabled.
func TestSSLPortOverridesPortThroughOptions(t *testing.T) {
	config := applyOptions(DefaultConfig(),
		WithHost("smtp.example.com"),
		WithPort(587),
		WithSSL(SSLConfig{Enable: true, Port: 465}),
	)
	if got, want := config.address(), "smtp.example.com:465"; got != want {
		t.Fatalf("address() = %q, want %q", got, want)
	}

	// Verify is disabled so construction never dials; the client's address is
	// the observable output of the same precedence rule.
	sender, err := NewFromOptions(
		WithHost("smtp.example.com"),
		WithPort(587),
		WithSSL(SSLConfig{Enable: true, Port: 465}),
		WithVerify(false),
	)
	if err != nil {
		t.Fatalf("NewFromOptions() error = %v", err)
	}
	client, ok := sender.(*Client)
	if !ok {
		t.Fatalf("NewFromOptions() returned %T, want *Client", sender)
	}
	t.Cleanup(func() { _ = client.Close() })
	if got, want := client.address, "smtp.example.com:465"; got != want {
		t.Fatalf("client address = %q, want %q", got, want)
	}
}
