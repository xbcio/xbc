package web_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// TestTLSListenerTerminatesTheHandshake closes the loop between web.tls and
// http.Request.TLS. The securityheaders middleware emits HSTS exactly when
// Request().TLS is non-nil (its own test covers that decision); what only a
// real listener can show is the fact underneath it: this Server, serving
// through an Engine adapter that never learns TLS exists, receives a request
// whose TLS field the standard library has populated.
//
// The listener is a real socket rather than an httptest recorder on purpose.
// A recorder hands the handler a request whose TLS field a test wrote by hand,
// which is exactly the step that could silently stop happening when the
// listener is not wrapped.
func TestTLSListenerTerminatesTheHandshake(t *testing.T) {
	certFile, keyFile, roots := writeTestCertificate(t)
	var (
		mu       sync.Mutex
		observed []tlsObservation
	)
	observer := fakeMiddleware{
		order: web.Order{Phase: web.PhaseObserve},
		handler: func(_ context.Context, c *web.Ctx) error {
			state := c.Request().TLS
			observation := tlsObservation{present: state != nil}
			if state != nil {
				observation.negotiatedProtocol = state.NegotiatedProtocol
				observation.version = state.Version
			}
			mu.Lock()
			observed = append(observed, observation)
			mu.Unlock()
			c.Next()
			return nil
		},
	}

	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.TLS = web.TLSConfig{CertFile: certFile, KeyFile: keyFile}
	capture := &captureLogger{}
	server, ctx, host := newPingServer(t, cfg, serverInputs{
		middlewares: []plugin.Entry[web.Middleware]{{Identity: plugin.Identity{Plugin: "tls-observer"}, Value: observer}},
	})
	host.logger = capture
	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))
	host.releaseTraffic()

	addr := server.Addr()
	// The client offers HTTP/2 as well as HTTP/1.1, which is what a browser
	// does. The server's NextProtos names only http/1.1, so the negotiated
	// protocol is the assertion that h2 is not on offer -- a client offering
	// nothing cannot tell the two apart.
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: roots, NextProtos: []string{"h2", "http/1.1"}},
		ForceAttemptHTTP2: false,
	}}
	var response *http.Response
	require.True(t, pollUntil(2*time.Second, 20*time.Millisecond, func() bool {
		resp, err := client.Get("https://" + addr + "/ping")
		if err != nil {
			return false
		}
		response = resp
		return true
	}), "the wrapped listener must answer an HTTPS request once the traffic gate opens")
	defer response.Body.Close()

	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, "HTTP/1.1", response.Proto,
		"NextProtos pins the offered protocol; an HTTPS request must not negotiate HTTP/2")
	require.NotNil(t, response.TLS)
	assert.Equal(t, "http/1.1", response.TLS.NegotiatedProtocol,
		"a client offering h2 must be answered with http/1.1, which is what makes the no-h2 limitation a decision instead of a default")
	mu.Lock()
	assert.Equal(t, []tlsObservation{{present: true, negotiatedProtocol: "http/1.1", version: tls.VersionTLS13}},
		observed,
		"the server must see Request().TLS populated on a TLS listener, with the pinned ALPN protocol")
	mu.Unlock()

	// A plaintext request to a TLS listener is refused by net/http itself, not
	// by any handler here: the standard library recognises an HTTP/1.x request
	// arriving in cleartext and answers 400. The assertion is about the status
	// rather than about a dial error, because the refusal is a response.
	plaintext, err := (&http.Client{Timeout: time.Second}).Get("http://" + addr + "/ping")
	require.NoError(t, err)
	defer plaintext.Body.Close()
	assert.Equal(t, http.StatusBadRequest, plaintext.StatusCode,
		"a listener terminating TLS must not serve a plaintext request")

	// The version floor is what min_version exists for; a client capped below
	// it must fail the handshake rather than be served at a lower version.
	oldClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, MaxVersion: tls.VersionTLS11},
	}}
	_, err = oldClient.Get("https://" + addr + "/ping")
	assert.Error(t, err, "min_version 1.2 must refuse a TLS 1.1 client")

	assert.Contains(t, strings.Join(capture.infos(), "\n"),
		"web: TLS termination enabled (min_version 1.2, client_auth none, cipher_suites default)",
		"the startup report must state the handshake policy an operator is about to serve")
}

// TestTLSMaterialIsValidatedWithoutBinding pins the split between the
// configuration validator, which is I/O-free, and the pipeline assembly, which
// reads the certificate and is shared by Start and Preflight. A missing or
// malformed file therefore fails both commands the same way -- and Preflight
// still binds nothing, which is the point of running it in CI.
func TestTLSMaterialIsValidatedWithoutBinding(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, _ := writeTestCertificate(t)
	malformedCA := filepath.Join(dir, "ca.crt")
	require.NoError(t, os.WriteFile(malformedCA, []byte("not a certificate"), 0o600))

	tests := []struct {
		name  string
		tls   web.TLSConfig
		match string
	}{
		{
			name:  "missing certificate",
			tls:   web.TLSConfig{CertFile: filepath.Join(dir, "absent.crt"), KeyFile: filepath.Join(dir, "absent.key")},
			match: "tls.cert_file",
		},
		{
			name: "malformed client CA bundle",
			tls: web.TLSConfig{
				CertFile:     certFile,
				KeyFile:      keyFile,
				ClientAuth:   "require_and_verify",
				ClientCAFile: malformedCA,
			},
			match: "tls.client_ca_file",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := web.DefaultConfig()
			cfg.Addr = "127.0.0.1:0"
			cfg.TLS = test.tls

			server, ctx, host := newPingServer(t, cfg, serverInputs{})
			err := server.Start(ctx)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.match)
			assert.Empty(t, server.Addr(), "a failed Start must not leave a bound listener behind")
			assert.Empty(t, host.submittedTasks(), "a failed Start must admit no serving task")

			preflight, preflightCtx, preflightHost := newPingServer(t, cfg, serverInputs{})
			err = preflight.Preflight(preflightCtx)
			require.Error(t, err, "validate must catch unreadable TLS material before a rollout does")
			assert.Contains(t, err.Error(), test.match)
			assert.Empty(t, preflight.Addr(), "Preflight must not bind the configured address")
			assert.Empty(t, preflightHost.submittedTasks())
		})
	}
}

// TestPlainHTTPStaysTheDefault is the regression this whole feature could most
// easily break: a deployment that never writes a web.tls section must serve
// exactly what it served before TLS existed, and its startup report must not
// gain a line about a capability it does not use.
func TestPlainHTTPStaysTheDefault(t *testing.T) {
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	capture := &captureLogger{}
	server, ctx, host := newPingServer(t, cfg, serverInputs{})
	host.logger = capture
	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))
	host.releaseTraffic()

	addr := server.Addr()
	require.True(t, pollUntil(2*time.Second, 20*time.Millisecond, func() bool {
		return probe(addr, "/ping", 250*time.Millisecond) == nil
	}), "an unwrapped listener must keep serving plain HTTP")

	assert.NotContains(t, strings.Join(capture.infos(), "\n"), "TLS",
		"a plain-HTTP deployment's report must be byte-identical to what it was before this process could terminate TLS")
}

// tlsObservation is what one request's handshake looked like from the
// server's side: the fact the securityheaders middleware reads, plus the
// negotiated protocol and version, so a misconfigured ALPN or version floor
// fails here rather than in a browser.
type tlsObservation struct {
	present            bool
	negotiatedProtocol string
	version            uint16
}

// writeTestCertificate writes a self-signed server certificate and its key to
// a temporary directory and returns the paths plus the pool a client has to
// trust to verify them. The certificate is generated in-process: a test that
// reached a certificate authority on the network would be testing the network.
func writeTestCertificate(t *testing.T) (certFile, keyFile string, roots *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	require.NoError(t, err)

	dir := t.TempDir()
	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	encodedKey, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encodedKey}), 0o600))

	roots = x509.NewCertPool()
	certificate, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	roots.AddCert(certificate)
	return certFile, keyFile, roots
}
