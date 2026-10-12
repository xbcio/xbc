package httpclient

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// minVersion resolves the configured version floor. An empty value means the
// default, so a directly constructed Config -- which never passed through tag
// defaulting -- builds without having to spell the default out.
func (t TLSConfig) minVersion() (uint16, error) {
	switch t.MinVersion {
	case "", defaultTLSMinVersion:
		return tls.VersionTLS12, nil
	case "1.3":
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("http-client: tls.min_version %q is not supported; use %q or %q", t.MinVersion, defaultTLSMinVersion, "1.3")
	}
}

// clientTLSConfig builds the *tls.Config outbound connections handshake with.
// A nil result with a nil error means "verify with the system roots and
// present nothing": an instance that configures no TLS material keeps Go's
// own defaults, which is what a deployment dialing a public endpoint wants.
//
// The material is loaded eagerly, so an unreadable CA bundle or certificate
// pair fails construction -- and therefore validate -- instead of surfacing
// as a handshake failure on the first production call.
func (t TLSConfig) clientTLSConfig() (*tls.Config, error) {
	if t.CAFile == "" && t.CertFile == "" && t.KeyFile == "" && t.ServerName == "" {
		return nil, nil
	}
	minVersion, err := t.minVersion()
	if err != nil {
		return nil, err
	}
	config := &tls.Config{
		MinVersion: minVersion,
		ServerName: t.ServerName,
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
			return nil, fmt.Errorf("http-client: tls.cert_file/tls.key_file: %w", err)
		}
		config.Certificates = []tls.Certificate{certificate}
	}
	return config, nil
}

// loadRootCAs reads the PEM bundle the peer is verified against. An empty or
// malformed bundle is an error rather than an empty pool: with an empty pool
// every peer certificate fails verification, so the process would construct
// cleanly and then refuse every call -- a failure an operator would have to
// infer from other people's connection errors.
func loadRootCAs(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("http-client: tls.ca_file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("http-client: tls.ca_file %s contains no PEM certificate", path)
	}
	return pool, nil
}
