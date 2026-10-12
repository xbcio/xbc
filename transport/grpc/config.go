package grpc

import (
	"fmt"
	"strings"
	"time"
)

const (
	defaultAddr                 = ":9090"
	defaultMaxRecvMsgBytes      = 4 << 20 // 4 MiB, gRPC's own receive default
	defaultMaxSendMsgBytes      = 4 << 20
	defaultMaxConcurrentStreams = 100
	defaultTLSMinVersion        = "1.2"
	defaultTLSClientAuth        = "none"
)

// Config is the root-level "grpc" configuration section. Like the Web
// transport's section, it deliberately carries no process-wide shutdown
// timeout: that budget belongs to the runtime, and Server.Stop drains only for
// the deadline it is handed.
//
// MaxRecvMsgBytes and MaxSendMsgBytes bound one message in either direction.
// gRPC's own send default is unbounded; the transport applies the same 4 MiB
// ceiling both ways so a handler that builds an oversized reply fails locally
// rather than on the peer. MaxConcurrentStreams is per connection -- the
// HTTP/2 setting a client is told to respect -- and defaults to 100, the value
// the HTTP/2 specification recommends as a floor, rather than gRPC's
// unbounded default.
type Config struct {
	Addr                 string           `yaml:"addr"                   default:":9090"   validate:"required"`
	MaxRecvMsgBytes      int              `yaml:"max_recv_msg_bytes"     default:"4194304" validate:"gt=0"`
	MaxSendMsgBytes      int              `yaml:"max_send_msg_bytes"     default:"4194304" validate:"gt=0"`
	MaxConcurrentStreams uint32           `yaml:"max_concurrent_streams" default:"100"     validate:"gt=0"`
	Shutdown             ShutdownConfig   `yaml:"shutdown"`
	Recovery             RecoveryConfig   `yaml:"recovery"`
	Security             SecurityConfig   `yaml:"security"`
	Reflection           ReflectionConfig `yaml:"reflection"`
	TLS                  TLSConfig        `yaml:"tls"`
}

// ReflectionConfig controls the standard gRPC server reflection service.
// Reflection publishes the full service and method schema to any caller that
// can reach the listener, so it is opt-in; enabling it serves both
// grpc.reflection.v1 and grpc.reflection.v1alpha.
type ReflectionConfig struct {
	Enabled bool `yaml:"enabled"`
}

// ShutdownConfig controls gRPC-owned behavior after runtime cancellation. The
// shared application shutdown deadline remains owned by the runtime; the
// transport never creates a second budget.
type ShutdownConfig struct {
	// PreDrainDelay keeps a serving listener accepting RPCs after lifecycle
	// contexts have been cancelled and before GracefulStop stops accepting new
	// ones. It gives a load balancer that watches a readiness signal a window
	// to stop routing to this instance. It is an opportunity, not confirmation
	// that routing has stopped.
	PreDrainDelay time.Duration `yaml:"pre_drain_delay" default:"0s" validate:"gte=0"`
}

// RecoveryConfig controls the Server-assembled panic boundary. There is no
// switch to disable it: the boundary is a required stage, which is why it is
// assembled rather than contributed.
type RecoveryConfig struct {
	// Stack includes a runtime stack in the panic boundary's log entry. Panic
	// values, request messages, and metadata are never logged regardless of
	// this setting.
	Stack bool `yaml:"stack" default:"true"`
}

// TLSConfig terminates TLS on the serving listener from a certificate pair on
// disk. An empty CertFile is the default and serves plaintext, which is the
// shape a deployment behind a TLS-terminating proxy -- or one whose peers are
// inside a trusted network -- writes; setting it is what moves termination
// into this process.
//
// ReloadInterval is how often the certificate pair is allowed to be checked
// for a change, at most once per handshake: zero -- the default -- checks on
// every handshake. A nonzero value bounds the check rate rather than the
// staleness of the certificate: a rotate is picked up by the first handshake
// after the interval has elapsed, while established connections keep the
// certificate they negotiated until they next complete a handshake.
type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// MinVersion is the lowest TLS version accepted, spelled "1.2" or "1.3".
	// TLS 1.0 and 1.1 are deliberately not addressable: they are deprecated by
	// RFC 8996, and a framework that let a deployment name them would be
	// offering a downgrade it knows to be unsafe.
	MinVersion string `yaml:"min_version" default:"1.2" validate:"omitempty,oneof=1.2 1.3"`
	// ClientCAFile is the PEM bundle client certificates are verified against.
	// It belongs with ClientAuth: a CA with no verification mode has nothing to
	// verify, and a mode with no CA could only accept certificates it cannot
	// validate.
	ClientCAFile string `yaml:"client_ca_file"`
	// ClientAuth is one of none, verify_if_given, or require_and_verify. The
	// two verify modes both require ClientCAFile. The modes Go names but this
	// section does not expose -- request, require_any -- accept a certificate
	// without verifying it, which is a state that reads as mutual TLS in a
	// review and authenticates nobody.
	ClientAuth string `yaml:"client_auth" default:"none" validate:"omitempty,oneof=none verify_if_given require_and_verify"`
	// CipherSuites restricts the TLS 1.2 cipher suites by Go name, for example
	// TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256. It has no effect on TLS 1.3,
	// whose suites are not configurable in Go; see tls.CipherSuites for why
	// the names accepted here are the secure set only. Empty -- the default --
	// uses Go's own defaults.
	CipherSuites   []string      `yaml:"cipher_suites" validate:"dive,required"`
	ReloadInterval time.Duration `yaml:"reload_interval" default:"0s" validate:"gte=0"`
}

// DefaultConfig returns the same defaults applied by XBC's config binder.
func DefaultConfig() Config {
	return Config{
		Addr:                 defaultAddr,
		MaxRecvMsgBytes:      defaultMaxRecvMsgBytes,
		MaxSendMsgBytes:      defaultMaxSendMsgBytes,
		MaxConcurrentStreams: defaultMaxConcurrentStreams,
		Recovery:             RecoveryConfig{Stack: true},
		Security:             SecurityConfig{Default: SecurityDeny},
		TLS: TLSConfig{
			MinVersion: defaultTLSMinVersion,
			ClientAuth: defaultTLSClientAuth,
		},
	}
}

// Validate checks constraints that cannot be expressed by configuration tags.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Addr) == "" || c.Addr != strings.TrimSpace(c.Addr) {
		return fmt.Errorf("grpc: addr must be a non-empty address without surrounding whitespace")
	}
	if c.MaxRecvMsgBytes <= 0 || c.MaxSendMsgBytes <= 0 {
		return fmt.Errorf("grpc: message size limits must be greater than zero")
	}
	if c.MaxConcurrentStreams == 0 {
		return fmt.Errorf("grpc: max_concurrent_streams must be greater than zero")
	}
	if c.Shutdown.PreDrainDelay < 0 {
		return fmt.Errorf("grpc: shutdown.pre_drain_delay must not be negative")
	}
	if err := c.Security.Validate(); err != nil {
		return err
	}
	return c.TLS.validate()
}

// normalizeConfig fills zero values a programmatic caller left unset and then
// validates the result, so New and the configured Definition agree on what a
// partially populated Config means.
func normalizeConfig(c Config) (Config, error) {
	defaults := DefaultConfig()
	if c.Addr == "" {
		c.Addr = defaults.Addr
	}
	if c.MaxRecvMsgBytes == 0 {
		c.MaxRecvMsgBytes = defaults.MaxRecvMsgBytes
	}
	if c.MaxSendMsgBytes == 0 {
		c.MaxSendMsgBytes = defaults.MaxSendMsgBytes
	}
	if c.MaxConcurrentStreams == 0 {
		c.MaxConcurrentStreams = defaults.MaxConcurrentStreams
	}
	// The TLS sub-struct gets the same treatment: its defaults are values a
	// directly constructed Config does not have to spell out.
	if c.TLS.MinVersion == "" {
		c.TLS.MinVersion = defaults.TLS.MinVersion
	}
	if c.TLS.ClientAuth == "" {
		c.TLS.ClientAuth = defaults.TLS.ClientAuth
	}
	c.TLS.CipherSuites = append([]string(nil), c.TLS.CipherSuites...)
	c.Security = c.Security.normalize()
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}
