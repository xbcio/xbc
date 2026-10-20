package web

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
)

// TestTLSConfigNextProtos pins the ALPN decision itself. The wire behavior is
// covered by the TLS tests beside the package, but those can only show the
// protocol a client ends up with; the list, its order, and the unchanged
// default are decided here.
func TestTLSConfigNextProtos(t *testing.T) {
	assert.Equal(t, []string{"http/1.1"}, TLSConfig{}.nextProtos(),
		"HTTP/2 is opt-in: the default must offer exactly the protocol this process served before the option existed")
	assert.Equal(t, []string{"h2", "http/1.1"}, TLSConfig{HTTP2: true}.nextProtos(),
		"h2 must be preferred, with http/1.1 kept as the fallback so clients that cannot speak it keep working")
}

// TestServerTLSConfigRefusesHTTP2WithoutACertificate is the direct-construction
// defense: an embedding host that builds a Config and calls Start without the
// binder never reaches the configuration validator, so the same refusal has to
// live in the builder rather than letting the process silently serve HTTP/1.1
// after being asked for HTTP/2. No file is read because the guard runs first.
func TestServerTLSConfigRefusesHTTP2WithoutACertificate(t *testing.T) {
	config, err := TLSConfig{HTTP2: true}.serverTLSConfig(log.Nop())

	require.Error(t, err)
	assert.Nil(t, config, "a refused configuration must not produce a listener config")
	assert.Equal(t, http2WithoutCertificateError.Error(), err.Error(),
		"the direct-construction path must report exactly what the config validator reports")
}

// TestManagementTLSConfigRefusesAClientCAWithoutServingTLS is the
// direct-construction defense for management.client_ca_file: Config.Validate
// refuses the pair, but a Config built past the validator must still fail with
// an explanation rather than panicking inside Clone, where there is no serving
// config to clone.
func TestManagementTLSConfigRefusesAClientCAWithoutServingTLS(t *testing.T) {
	config, err := managementTLSConfig(nil, "/etc/tls/management-ca.pem")

	require.Error(t, err)
	assert.Nil(t, config, "a refused configuration must not produce a listener config")
	assert.Contains(t, err.Error(), "management.client_ca_file")
	assert.Contains(t, err.Error(), "web.tls.cert_file")
}

// TestServerTLSConfigRefusesHTTP2WithACBCSuite covers the other refusable h2
// shape on the direct-construction path. The CBC check lives in cipherSuiteIDs,
// which the builder calls before it touches the filesystem, so the fake paths
// below are never read -- what the test pins is that an embedding host reaches
// the same refusal without passing through the binder.
func TestServerTLSConfigRefusesHTTP2WithACBCSuite(t *testing.T) {
	config, err := TLSConfig{
		CertFile:     "/etc/tls/tls.crt",
		KeyFile:      "/etc/tls/tls.key",
		HTTP2:        true,
		CipherSuites: []string{"TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA"},
	}.serverTLSConfig(log.Nop())

	require.Error(t, err)
	assert.Nil(t, config, "a refused configuration must not produce a listener config")
	assert.Equal(t,
		"web: tls.cipher_suites names \"TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA\", which HTTP/2 prohibits (RFC 9113 Appendix A)\n"+
			"  → the standard library's HTTP/2 server resets every connection that negotiates this TLS 1.2 CBC suite; remove the suite, set tls.http2 to false, or raise tls.min_version to 1.3",
		err.Error(),
		"the direct-construction path must report exactly what the config validator reports")
}
