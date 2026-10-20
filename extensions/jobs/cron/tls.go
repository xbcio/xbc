package cron

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// defaultTLSMinVersion is the floor a section gets without declaring one. TLS
// 1.0 and 1.1 are deliberately not addressable: they are deprecated by RFC
// 8996, and a framework that let a deployment name them would offer a
// downgrade it knows to be unsafe.
const defaultTLSMinVersion = "1.2"

// TLSConfig enables TLS on the Redis connection cron owns and optionally
// supplies the material it verifies and presents.
//
// The zero value means "no TLS": the connection stays exactly as it was before
// the section existed. Setting any of ca_file, cert_file, key_file,
// server_name, or insecure_skip_verify turns TLS on. min_version does not
// participate in that decision because its default tag fills "1.2" into every
// bound section, which would otherwise turn TLS on for deployments that never
// asked for it.
type TLSConfig struct {
	// CAFile is the PEM bundle the server is verified against. Empty uses the
	// system roots.
	CAFile string `yaml:"ca_file"`

	// CertFile and KeyFile are the client certificate pair presented when the
	// server asks for one (mTLS). They must be configured together.
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`

	// ServerName overrides the name verified against the server certificate's
	// SANs, for the deployment whose dialed address is not the certificate's.
	ServerName string `yaml:"server_name"`

	// MinVersion is the lowest TLS version accepted, spelled "1.2" or "1.3".
	MinVersion string `yaml:"min_version" default:"1.2" validate:"omitempty,oneof=1.2 1.3"`

	// InsecureSkipVerify disables certificate verification. It exists for
	// development endpoints and must not be used in production: it turns TLS
	// into encryption without authentication, which is indistinguishable from
	// cleartext against an active attacker.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify" default:"false"`
}

// configured reports whether the section asks for TLS at all.
func (t TLSConfig) configured() bool {
	return t.CAFile != "" || t.CertFile != "" || t.KeyFile != "" || t.ServerName != "" || t.InsecureSkipVerify
}

// validate enforces the invariants that are decidable from the section alone.
// It performs no I/O, so it runs during planning, before construction.
func (t TLSConfig) validate() error {
	if (t.CertFile == "") != (t.KeyFile == "") {
		return fmt.Errorf("cron: distributed.redis.tls.cert_file and distributed.redis.tls.key_file must be configured together")
	}
	switch t.MinVersion {
	case "", defaultTLSMinVersion, "1.3":
	default:
		return fmt.Errorf(
			"cron: distributed.redis.tls.min_version %q is not supported; use %q or %q",
			t.MinVersion, defaultTLSMinVersion, "1.3",
		)
	}
	return nil
}

// clientTLSConfig builds the *tls.Config the owned client dials with. A nil
// result with a nil error means "plaintext": the section configured no TLS.
//
// The material is loaded eagerly, so an unreadable CA bundle or certificate
// pair fails construction instead of surfacing as a handshake failure on the
// first lock operation.
func (t TLSConfig) clientTLSConfig() (*tls.Config, error) {
	if err := t.validate(); err != nil {
		return nil, err
	}
	if !t.configured() {
		return nil, nil
	}

	minVersion := uint16(tls.VersionTLS12)
	if t.MinVersion == "1.3" {
		minVersion = tls.VersionTLS13
	}
	config := &tls.Config{
		MinVersion:         minVersion,
		ServerName:         t.ServerName,
		InsecureSkipVerify: t.InsecureSkipVerify, //nolint:gosec // explicitly configured for development endpoints
	}
	if t.CAFile != "" {
		pool, err := loadRootCAs(t.CAFile)
		if err != nil {
			return nil, err
		}
		config.RootCAs = pool
	}
	if t.CertFile != "" {
		certificate, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("cron: distributed.redis.tls.cert_file/distributed.redis.tls.key_file: %w", err)
		}
		config.Certificates = []tls.Certificate{certificate}
	}
	return config, nil
}

// loadRootCAs reads the PEM bundle the server is verified against. An empty or
// malformed bundle is an error rather than an empty pool: with an empty pool
// every server certificate fails verification, so the process would construct
// cleanly and then refuse every connection.
func loadRootCAs(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cron: distributed.redis.tls.ca_file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("cron: distributed.redis.tls.ca_file %s contains no PEM certificate", path)
	}
	return pool, nil
}
