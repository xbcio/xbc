package management

import (
	"crypto/tls"
	"fmt"
	"net"
	"strings"
)

// defaultTLSMinVersion is the floor a deployment gets without declaring one.
// It matches the gRPC serving listener's own default, so both listeners of a
// deployment negotiate the same floor.
const defaultTLSMinVersion = "1.2"

// Config is the plugins.grpc-management configuration section. The section's
// presence is the activation -- there is no enabled key -- and an address is
// required with it, because a section that asks for a management listener
// without saying where to bind one is a configuration mistake rather than a
// request for the default behavior.
//
// Access control on the management listener is the bound address itself, not
// an authentication policy: an operator surface with no business identity has
// no principal to authenticate. A hostless address (":9091") therefore binds
// the loopback interface, a host that is not loopback requires AllowRemote,
// and AllowRemote in turn requires a serving certificate, so a management port
// is never exposed to a network unencrypted.
type Config struct {
	// Addr is the listen address of the management listener, for example
	// "127.0.0.1:9091" or ":9091". See bindAddr for why the hostless spelling
	// binds loopback rather than every interface.
	Addr string `yaml:"addr" validate:"omitempty,hostname_port"`
	// AllowRemote permits binding a management address that is not loopback.
	// It is refused without TLS.CertFile, because the alternative -- an
	// unauthenticated operator surface reachable from the network in
	// cleartext -- is never what an operator meant to configure.
	AllowRemote bool `yaml:"allow_remote"`
	// Pprof serves Go's standard profiling handlers under /debug/pprof/. It is
	// off by default: a profile is a debugging action an operator opts into for
	// the incident at hand, not a surface every deployment leaves open.
	Pprof bool `yaml:"pprof"`
	// TLS terminates TLS on the management listener from a certificate pair on
	// disk. It is the listener's own section rather than a share of the gRPC
	// server's: the two are configured independently, so rotating the serving
	// certificate cannot silently change what the operator surface presents.
	TLS TLSConfig `yaml:"tls"`
}

// TLSConfig terminates TLS on the management listener. An empty CertFile is
// the default and serves plain HTTP, which is what the loopback-only default
// address makes safe; AllowRemote is refused without it.
type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// MinVersion is the lowest TLS version accepted, spelled "1.2" or "1.3".
	// TLS 1.0 and 1.1 are deliberately not addressable: they are deprecated by
	// RFC 8996, and a framework that let a deployment name them would be
	// offering a downgrade it knows to be unsafe.
	MinVersion string `yaml:"min_version" default:"1.2" validate:"omitempty,oneof=1.2 1.3"`
}

// DefaultConfig returns the same defaults applied by XBC's config binder.
func DefaultConfig() Config {
	return Config{TLS: TLSConfig{MinVersion: defaultTLSMinVersion}}
}

// Validate checks constraints that cannot be expressed by configuration tags.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Addr) == "" {
		return fmt.Errorf(
			"grpc-management: addr must be a non-empty address\n" +
				"  → the plugins.grpc-management section is what asks for a management listener; remove the section to run without one",
		)
	}
	if c.Addr != strings.TrimSpace(c.Addr) {
		return fmt.Errorf("grpc-management: addr must not be padded with whitespace")
	}
	host, port, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return fmt.Errorf("grpc-management: addr %q is not a host:port address: %w", c.Addr, err)
	}
	if port == "" {
		return fmt.Errorf("grpc-management: addr %q must name a port", c.Addr)
	}
	if !managementHostIsLoopback(host) && !c.AllowRemote {
		return fmt.Errorf(
			"grpc-management: addr %q binds an address that is not loopback\n"+
				"  → the management listener carries no business authentication, so its access control is the bound address; set allow_remote: true (which requires tls.cert_file) to expose it, or leave the host out to bind loopback",
			c.Addr)
	}
	if c.AllowRemote && strings.TrimSpace(c.TLS.CertFile) == "" {
		return fmt.Errorf(
			"grpc-management: allow_remote requires tls.cert_file\n" +
				"  → a remotely reachable management listener is an unauthenticated operator surface; the certificate is what keeps it off a cleartext network",
		)
	}
	return c.TLS.validate()
}

// validate checks the TLS material's declarations. It does not read the
// certificate: whether the files exist and parse is a startup question with a
// startup error, answered where the listener is bound (see material), not by a
// configuration validator whose contract is to stay I/O-free.
func (c TLSConfig) validate() error {
	certFile := strings.TrimSpace(c.CertFile)
	keyFile := strings.TrimSpace(c.KeyFile)
	if (certFile == "") != (keyFile == "") {
		return fmt.Errorf("grpc-management: tls.cert_file and tls.key_file must be set together")
	}
	if c.CertFile != certFile || c.KeyFile != keyFile {
		return fmt.Errorf("grpc-management: tls.cert_file and tls.key_file must not be padded with whitespace")
	}
	if certFile == "" {
		return nil
	}
	_, err := c.minVersion()
	return err
}

// minVersion resolves the configured TLS floor to the crypto/tls constant. An
// empty value resolves to the default rather than zero, so a directly
// constructed Config cannot accidentally request Go's "no floor" behavior.
func (c TLSConfig) minVersion() (uint16, error) {
	switch c.MinVersion {
	case "", defaultTLSMinVersion:
		return tls.VersionTLS12, nil
	case "1.3":
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("grpc-management: tls.min_version %q must be \"1.2\" or \"1.3\"", c.MinVersion)
	}
}

// material loads the certificate pair, or returns nil when none is
// configured. The error names both keys rather than the failing file: which of
// the pair was rejected is rarely the useful part, and the two are always
// rotated together.
func (c TLSConfig) material() (*tls.Config, error) {
	if strings.TrimSpace(c.CertFile) == "" {
		return nil, nil
	}
	version, err := c.minVersion()
	if err != nil {
		return nil, err
	}
	certificate, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("grpc-management: tls.cert_file/tls.key_file: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   version,
	}, nil
}

// managementHostIsLoopback reports whether a management address's host part
// names this machine only. An empty host -- the ":9091" spelling, which a bare
// net.Listen would bind to every interface -- counts as loopback because
// bindAddr resolves it to the loopback interface before listening;
// "localhost" counts because it resolves to the loopback interface. Every
// other host, IP or name, is treated as remote: a hostname this process cannot
// resolve offline might point anywhere, and the conservative reading is the
// one that demands explicit consent.
//
// The rule is the same one the Web management listener applies, restated here
// because the two transports share no code; a change to either is a change to
// the framework's binding policy for operator ports.
func managementHostIsLoopback(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// bindAddr resolves Addr to the address the listener actually binds. The
// resolution exists because a hostless spelling does not mean what it looks
// like once it reaches net.Listen: ":9091" binds every interface, which on
// this listener would publish an operator surface that carries no
// authentication to the network. Filling the missing host with loopback is
// what makes managementHostIsLoopback's reading of an empty host true rather
// than aspirational -- a config that validated as loopback-only must be
// reachable from this machine only.
//
// An address that already names a host is passed through untouched, including
// the explicit remote spellings allow_remote exists to consent to.
func (c Config) bindAddr() string {
	host, port, err := net.SplitHostPort(c.Addr)
	if err != nil || host != "" {
		return c.Addr
	}
	return net.JoinHostPort("127.0.0.1", port)
}

// normalizeConfig fills zero values a programmatic caller left unset and then
// validates the result, so New and the configured Definition agree on what a
// partially populated Config means.
func normalizeConfig(c Config) (Config, error) {
	if c.TLS.MinVersion == "" {
		c.TLS.MinVersion = defaultTLSMinVersion
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}
