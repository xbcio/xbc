package web

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"strings"
)

// serverTLSConfig builds the *tls.Config this process terminates TLS with. A
// nil result with a nil error is the default and means "serve plain HTTP":
// web.tls.cert_file being unset is what a deployment behind a terminating
// proxy writes, or rather does not write.
//
// Termination happens here, on the listener, rather than through a
// net/http.Server TLSConfig: the Engine port hands the Server one already
// built listener and the engine owns the *http.Server inside its own adapter,
// so tls.NewListener is the only seam that adds TLS without giving the Engine
// port a TLS-specific method. net/http recognises a TLS listener, so
// http.Request.TLS is populated by the standard library -- which is what lets
// the securityheaders middleware emit HSTS for a request that really did
// arrive over TLS, with no code of ours involved.
//
// The cost of that seam is stated rather than hidden: ALPN is negotiated by
// the config built here, and NextProtos is pinned to "http/1.1", so this
// process serves HTTP/1.1 over TLS and does not negotiate HTTP/2.
func (c TLSConfig) serverTLSConfig() (*tls.Config, error) {
	if strings.TrimSpace(c.CertFile) == "" {
		return nil, nil
	}
	certificate, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("xbc: web tls.cert_file/tls.key_file: %w", err)
	}
	minVersion, err := c.minVersion()
	if err != nil {
		return nil, err
	}
	clientAuth, err := c.clientAuth()
	if err != nil {
		return nil, err
	}
	cipherSuites, err := c.cipherSuiteIDs()
	if err != nil {
		return nil, err
	}

	config := &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   minVersion,
		CipherSuites: cipherSuites,
		ClientAuth:   clientAuth,
		NextProtos:   []string{"http/1.1"},
	}
	if path := strings.TrimSpace(c.ClientCAFile); path != "" {
		pool, err := loadClientCAs(path)
		if err != nil {
			return nil, err
		}
		config.ClientCAs = pool
	}
	return config, nil
}

// loadClientCAs reads the PEM bundle client certificates are verified
// against. An empty or malformed bundle is an error rather than an empty pool:
// with an empty pool every client certificate fails verification, so a
// deployment would start and then reject every client at the handshake -- a
// failure an operator would have to infer from other people's connection
// errors.
func loadClientCAs(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("xbc: web tls.client_ca_file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("xbc: web tls.client_ca_file %s contains no PEM certificate", path)
	}
	return pool, nil
}

// wrapTLS wraps the bound listener when the section declares a certificate.
// The wrapper is what Engine.Serve receives, so the engine never learns
// whether it is serving TLS: it accepts connections from a listener exactly as
// it always has.
func wrapTLS(ln net.Listener, config *tls.Config) net.Listener {
	if config == nil {
		return ln
	}
	return tls.NewListener(ln, config)
}

// renderTLS reports that this process terminates TLS and with which policy,
// so an operator reading a boot confirms what the handshake will accept
// instead of inferring it from a proxy's configuration elsewhere. The line
// is printed only when the section declares a certificate: a plain-HTTP
// deployment's report is byte-identical to what it was before this process
// could terminate TLS at all, which is what keeps the default shape
// reviewable.
func renderTLS(cfg TLSConfig) string {
	if strings.TrimSpace(cfg.CertFile) == "" {
		return ""
	}
	return fmt.Sprintf(
		"web: TLS termination enabled (min_version %s, client_auth %s, cipher_suites %s)",
		cfg.MinVersion,
		cfg.ClientAuth,
		renderCipherSuites(cfg.CipherSuites),
	)
}

// renderCipherSuites names the declared suites, or says that Go's defaults
// apply. The count is deliberately not printed: which suites are in force is
// the fact an operator is checking, and a list is short by construction.
func renderCipherSuites(suites []string) string {
	if len(suites) == 0 {
		return "default"
	}
	return strings.Join(suites, ",")
}
