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
		"web: TLS termination enabled (min_version 1.2, client_auth none, cipher_suites default, reload_interval 0s)",
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

	certFile, keyFile, certificate := writeCertificatePair(t, t.TempDir(), 1)
	roots = x509.NewCertPool()
	roots.AddCert(certificate)
	return certFile, keyFile, roots
}

// writeCertificatePair writes a fresh self-signed pair with the given serial
// into dir, overwriting whatever pair is already there -- which is what a
// rotation does. The modification time is set explicitly rather than left to
// the filesystem's clock: the reload trigger is an identity of modification
// time and size, and two writes inside one filesystem timestamp tick would
// otherwise make a rotation invisible to the code under test.
func writeCertificatePair(t *testing.T, dir string, serial int64) (certFile, keyFile string, certificate *x509.Certificate) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := x509.Certificate{
		SerialNumber:          big.NewInt(serial),
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

	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	encodedKey, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encodedKey}), 0o600))

	stamp := time.Now().Add(time.Duration(serial) * time.Second)
	require.NoError(t, os.Chtimes(certFile, stamp, stamp))
	require.NoError(t, os.Chtimes(keyFile, stamp, stamp))

	certificate, err = x509.ParseCertificate(der)
	require.NoError(t, err)
	return certFile, keyFile, certificate
}

// certificateSerial reads which rotation a loaded certificate is. The serial
// is the only fact that distinguishes two certificates the same key generated,
// so it is what a rotation test asserts on.
func certificateSerial(t *testing.T, certificate *tls.Certificate) int64 {
	t.Helper()

	require.NotNil(t, certificate)
	require.NotEmpty(t, certificate.Certificate)
	parsed, err := x509.ParseCertificate(certificate.Certificate[0])
	require.NoError(t, err)
	return parsed.SerialNumber.Int64()
}

// newTLSClient builds a client that verifies the generated certificate and
// opens a fresh connection per request, so every request is a handshake -- the
// only place a rotation can be observed.
func newTLSClient(roots *x509.CertPool) *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: roots, NextProtos: []string{"h2", "http/1.1"}},
		ForceAttemptHTTP2: false,
		DisableKeepAlives: true,
	}}
}

// servedSerial performs one request and reports the serial of the certificate
// the server presented. A client that trusts only the pre-rotation
// certificate would fail verification after a rotation, so the pool a
// rotation test trusts carries every certificate it writes.
func servedSerial(t *testing.T, client *http.Client, addr string) int64 {
	t.Helper()

	response, err := client.Get("https://" + addr + "/ping")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NotNil(t, response.TLS)
	require.NotEmpty(t, response.TLS.PeerCertificates)
	return response.TLS.PeerCertificates[0].SerialNumber.Int64()
}

// servingFixture is a running Server that terminates TLS with a certificate
// pair this test owns and rotates. Each test builds its own -- a shared one
// would let one test's rotation decide what another test serves.
type servingFixture struct {
	dir      string
	certFile string
	keyFile  string
	addr     string
	client   *http.Client
	roots    *x509.CertPool
	logger   *captureLogger
}

// newServingFixture starts a Server on a generated pair with the given reload
// interval and waits until it answers, so a test's first assertion is about a
// certificate rather than about readiness.
func newServingFixture(t *testing.T, reloadInterval time.Duration) *servingFixture {
	t.Helper()

	dir := t.TempDir()
	certFile, keyFile, certificate := writeCertificatePair(t, dir, 1)

	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.TLS = web.TLSConfig{CertFile: certFile, KeyFile: keyFile, ReloadInterval: reloadInterval}
	capture := &captureLogger{}
	server, ctx, host := newPingServer(t, cfg, serverInputs{})
	host.logger = capture
	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))
	host.releaseTraffic()

	fixture := &servingFixture{
		dir:      dir,
		certFile: certFile,
		keyFile:  keyFile,
		addr:     server.Addr(),
		roots:    x509.NewCertPool(),
		logger:   capture,
	}
	fixture.roots.AddCert(certificate)
	fixture.client = newTLSClient(fixture.roots)
	require.True(t, pollUntil(2*time.Second, 20*time.Millisecond, func() bool {
		response, err := fixture.client.Get("https://" + fixture.addr + "/ping")
		if err != nil {
			return false
		}
		response.Body.Close()
		return true
	}), "the TLS listener must answer once the traffic gate opens")
	return fixture
}

// rotate replaces the serving pair with a new one, the way a deployment does
// when it renews, and starts trusting it: the client pool is read at handshake
// time, so the next request verifies against both certificates and can tell a
// rotation apart from a refusal.
func (f *servingFixture) rotate(t *testing.T, serial int64) {
	t.Helper()

	_, _, certificate := writeCertificatePair(t, f.dir, serial)
	f.roots.AddCert(certificate)
}

// serial reports which certificate the next connection is served.
func (f *servingFixture) serial(t *testing.T) int64 {
	t.Helper()
	return servedSerial(t, f.client, f.addr)
}

// TestCertificateRotationIsPickedUpOnTheNextHandshake is the whole point of
// GetCertificate, observed where an operator would observe it: a deployment
// replaces the files under a running process, and the next connection is
// served the new certificate. reload_interval 0 checks the file on every
// handshake, which is the default a rotation written by an external tool --
// a cert-manager, a sidecar, a kubectl create -- relies on.
func TestCertificateRotationIsPickedUpOnTheNextHandshake(t *testing.T) {
	fixture := newServingFixture(t, 0)
	assert.Equal(t, int64(1), fixture.serial(t), "the pair present at startup is what serves")
	assert.NotContains(t, strings.Join(fixture.logger.infos(), "\n"), "reloaded",
		"a pair that has not moved is not a rotation: reload_interval 0 asks the question every handshake, and the answer must not be a log line every handshake")

	fixture.rotate(t, 2)
	assert.Equal(t, int64(2), fixture.serial(t),
		"a rotated certificate must be presented without a restart")
	assert.Contains(t, strings.Join(fixture.logger.infos(), "\n"), "web: TLS certificate reloaded",
		"a rotation an operator cannot see in the log is a rotation they cannot audit")
	assert.Empty(t, fixture.logger.errors(), "a rotation that worked is not an error")
}

// TestBrokenRotationKeepsServingThePreviousCertificate pins what happens while
// a rotation is half done: the certificate file has been replaced, but not yet
// with a pair that loads. Failing the handshake would turn a rotation mistake
// into an outage at exactly the moment the previous certificate is still
// valid, so the old one keeps serving -- and the failure is reported once for
// the file state that caused it, not once per connection.
func TestBrokenRotationKeepsServingThePreviousCertificate(t *testing.T) {
	fixture := newServingFixture(t, 0)
	require.Equal(t, int64(1), fixture.serial(t))

	halfWritten := []byte("-----BEGIN CERTIFICATE-----\nnot a finished rotation\n")
	require.NoError(t, os.WriteFile(fixture.certFile, halfWritten, 0o600))
	touch(t, fixture.certFile, time.Hour)

	assert.Equal(t, int64(1), fixture.serial(t))
	assert.Equal(t, int64(1), fixture.serial(t), "every handshake inside a broken state keeps serving")
	require.Len(t, fixture.logger.errors(), 1,
		"a file that has not moved is one failure, however many connections it survives")
	assert.Contains(t, fixture.logger.errors()[0], "the previous certificate is still serving")
	assert.NotContains(t, strings.Join(fixture.logger.infos(), "\n"), "reloaded",
		"a failed load must not be reported as a rotation")

	// A different broken file is a different fact: an operator debugging a
	// second failed rotation has to see it.
	require.NoError(t, os.WriteFile(fixture.certFile, append(halfWritten, 'x'), 0o600))
	touch(t, fixture.certFile, 2*time.Hour)
	assert.Equal(t, int64(1), fixture.serial(t))
	assert.Len(t, fixture.logger.errors(), 2)

	fixture.rotate(t, 3)
	assert.Equal(t, int64(3), fixture.serial(t), "a completed rotation is served once the file loads")
	assert.Contains(t, strings.Join(fixture.logger.infos(), "\n"), "web: TLS certificate reloaded")
	assert.Len(t, fixture.logger.errors(), 2, "a recovery does not erase the failures before it")
}

// TestMissingCertificateIsReportedOnce covers the other way a rotation can
// fail: the file is gone, so there is no identity to compare and the failure
// has to be reported on the state that remains. Without that, a refused
// rotation that deletes and rewrites the certificate -- a rename-based rollout
// in its window -- would log an error per connection.
func TestMissingCertificateIsReportedOnce(t *testing.T) {
	fixture := newServingFixture(t, 0)
	require.Equal(t, int64(1), fixture.serial(t))

	require.NoError(t, os.Remove(fixture.certFile))

	assert.Equal(t, int64(1), fixture.serial(t))
	assert.Equal(t, int64(1), fixture.serial(t))
	assert.Len(t, fixture.logger.errors(), 1,
		"a certificate that stays gone stays one report")
}

// TestReloadIntervalBoundsHowOftenTheFileIsChecked pins the rate limit through
// the source's own clock. The certificate rotates between two lookups, and the
// interval -- not the write -- is what decides whether the rotation is seen;
// advancing a fake clock proves that without making the test wait, and without
// the flakiness a real interval under load would introduce.
func TestReloadIntervalBoundsHowOftenTheFileIsChecked(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, _ := writeCertificatePair(t, dir, 1)
	capture := &captureLogger{}
	source, err := web.NewCertificateSource(web.TLSConfig{
		CertFile:       certFile,
		KeyFile:        keyFile,
		ReloadInterval: time.Hour,
	}, capture)
	require.NoError(t, err)

	clock := time.Now()
	source.SetClock(func() time.Time { return clock })
	assert.Equal(t, int64(1), certificateSerial(t, source.ServeCertificate()))

	_, _, rotated := writeCertificatePair(t, dir, 2)
	require.Equal(t, int64(2), rotated.SerialNumber.Int64())

	assert.Equal(t, int64(1), certificateSerial(t, source.ServeCertificate()),
		"a handshake inside the interval the previous check claimed must not state the file")
	assert.Equal(t, int64(1), certificateSerial(t, source.ServeCertificate()))
	assert.NotContains(t, strings.Join(capture.infos(), "\n"), "reloaded")

	clock = clock.Add(time.Hour + time.Second)
	assert.Equal(t, int64(2), certificateSerial(t, source.ServeCertificate()),
		"the first handshake after the interval is the one that sees the rotation")
	assert.Contains(t, strings.Join(capture.infos(), "\n"), "web: TLS certificate reloaded")
}

// touch moves a file's modification time, which is half of the identity the
// reload trigger compares. Writing alone is not enough on a filesystem whose
// timestamps are coarse enough that two writes land in the same tick.
func touch(t *testing.T, path string, offset time.Duration) {
	t.Helper()
	stamp := time.Now().Add(offset)
	require.NoError(t, os.Chtimes(path, stamp, stamp))
}

// writeClientCA writes one certificate authority a server can verify client
// certificates against, and returns its PEM path plus the material a test
// signs a client certificate with. name lets one test write two of them -- the
// authority a deployment configured and one it is expected not to trust.
func writeClientCA(t *testing.T, dir, name string) (caFile string, ca *x509.Certificate, caKey *ecdsa.PrivateKey) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := x509.Certificate{
		SerialNumber:          big.NewInt(100),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	require.NoError(t, err)

	caFile = filepath.Join(dir, name+".crt")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	ca, err = x509.ParseCertificate(der)
	require.NoError(t, err)
	return caFile, ca, key
}

// writeClientCertificate issues a client certificate signed by ca, in the form
// a client presents during a handshake. The client extended key usage is what
// makes it a client certificate rather than another server's; a verifier is
// entitled to reject the latter.
func writeClientCertificate(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, serial int64) tls.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "xbc test client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, ca, &key.PublicKey, caKey)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// TestMutualTLSRequiresAVerifiedClientCertificate pins the half of web.tls that
// only a handshake can show: client_auth and client_ca_file reach the TLS
// configuration as a verification policy, not merely as validated strings. The
// accepted client is what makes the two refusals evidence -- a server that
// refused every client would satisfy both negative assertions.
func TestMutualTLSRequiresAVerifiedClientCertificate(t *testing.T) {
	dir := t.TempDir()
	caFile, ca, caKey := writeClientCA(t, dir, "client-ca")
	_, lookalikeCA, lookalikeKey := writeClientCA(t, t.TempDir(), "client-ca")
	certFile, keyFile, roots := writeTestCertificate(t)

	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.TLS = web.TLSConfig{
		CertFile:     certFile,
		KeyFile:      keyFile,
		ClientCAFile: caFile,
		ClientAuth:   "require_and_verify",
	}
	server, ctx, host := newPingServer(t, cfg, serverInputs{})
	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))
	host.releaseTraffic()
	addr := server.Addr()

	client := func(certificates ...tls.Certificate) *http.Client {
		return &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig:   &tls.Config{RootCAs: roots, Certificates: certificates, NextProtos: []string{"http/1.1"}},
				ForceAttemptHTTP2: false,
			},
		}
	}

	presented := client(writeClientCertificate(t, ca, caKey, 1))
	var response *http.Response
	require.True(t, pollUntil(2*time.Second, 20*time.Millisecond, func() bool {
		resp, err := presented.Get("https://" + addr + "/ping")
		if err != nil {
			return false
		}
		response = resp
		return true
	}), "a client certificate signed by tls.client_ca_file must complete the handshake")
	defer response.Body.Close()
	assert.Equal(t, http.StatusOK, response.StatusCode)
	require.NotNil(t, response.TLS)
	// The served response is the evidence: under require_and_verify the
	// standard library completes the handshake only after it has verified the
	// client's certificate against tls.client_ca_file, so a served request is
	// a verified client. (PeerCertificates on the client side name the server,
	// which is the other end of the same verification.)

	_, err := client().Get("https://" + addr + "/ping")
	assert.Error(t, err, "require_and_verify must refuse a client that presents no certificate")

	// A client picks the certificate to present by issuer name, so the
	// authority below carries the configured one's subject with a different
	// key: its certificate is really offered to the server and can only be
	// refused by verifying its signature. This is the half the case above does
	// not reach -- there, nothing was presented at all -- and it is what
	// separates a server that verifies from one that merely demands.
	_, err = client(writeClientCertificate(t, lookalikeCA, lookalikeKey, 2)).Get("https://" + addr + "/ping")
	assert.Error(t, err, "a certificate whose issuer matches by name but not by key must fail verification")
}
