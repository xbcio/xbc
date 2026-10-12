package grpc

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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// recordingLogger records the lines the transport logs, by level, so a test
// can assert on what an operator would read. Every method is implemented: an
// embedded nil Logger would panic on the first line nobody expected.
type recordingLogger struct {
	mu     sync.Mutex
	infos  []string
	warns  []string
	errors []string
}

func (*recordingLogger) Debug(string, ...any) {}

func (l *recordingLogger) Info(msg string, _ ...any)  { l.append(&l.infos, msg) }
func (l *recordingLogger) Warn(msg string, _ ...any)  { l.append(&l.warns, msg) }
func (l *recordingLogger) Error(msg string, _ ...any) { l.append(&l.errors, msg) }
func (*recordingLogger) Fatal(msg string, _ ...any)   { panic("recordingLogger.Fatal: " + msg) }
func (l *recordingLogger) With(_ ...any) log.Logger   { return l }
func (*recordingLogger) Enabled(log.Level) bool       { return true }

func (l *recordingLogger) append(destination *[]string, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	*destination = append(*destination, msg)
}

func (l *recordingLogger) infoLines() []string  { return l.snapshot(&l.infos) }
func (l *recordingLogger) errorLines() []string { return l.snapshot(&l.errors) }

func (l *recordingLogger) snapshot(source *[]string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), *source...)
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
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}

	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	encodedKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encodedKey}), 0o600); err != nil {
		t.Fatal(err)
	}

	stamp := time.Now().Add(time.Duration(serial) * time.Second)
	if err := os.Chtimes(certFile, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(keyFile, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	certificate, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, certificate
}

// writeTestCertificate writes a self-signed server certificate and its key to
// a temporary directory and returns the paths plus the pool a client has to
// trust to verify them.
func writeTestCertificate(t *testing.T) (certFile, keyFile string, roots *x509.CertPool) {
	t.Helper()

	certFile, keyFile, certificate := writeCertificatePair(t, t.TempDir(), 1)
	roots = x509.NewCertPool()
	roots.AddCert(certificate)
	return certFile, keyFile, roots
}

// writeClientCA writes one certificate authority a server can verify client
// certificates against, and returns its PEM path plus the material a test
// signs a client certificate with. name lets one test write two of them -- the
// authority a deployment configured and one it is expected not to trust.
func writeClientCA(t *testing.T, dir, name string) (caFile string, ca *x509.Certificate, caKey *ecdsa.PrivateKey) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}

	caFile = filepath.Join(dir, name+".crt")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return caFile, ca, key
}

// writeClientCertificate issues a client certificate signed by ca, in the form
// a client presents during a handshake.
func writeClientCertificate(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, serial int64) tls.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "xbc test client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// tlsClientConfig is what a gRPC client verifies the generated certificate
// with. The server name is the certificate's own, so verification is real.
func tlsClientConfig(roots *x509.CertPool) *tls.Config {
	return &tls.Config{RootCAs: roots, ServerName: "localhost"}
}

// serveRecorded starts s with a recording logger attached to its host and
// returns the recorder and a client connection dialed with the given option.
// The logger is in place before Start, so the startup report the assembly
// prints is recorded too.
func serveRecorded(t *testing.T, s *Server, dialOption grpc.DialOption) (*recordingLogger, *grpc.ClientConn) {
	t.Helper()
	s.listener = listenLoopback(t)
	recorder := &recordingLogger{}
	host := newFakeHost()
	host.logger = recorder
	ctx := contextFromHost(host)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.OpenTraffic(ctx); err != nil {
		t.Fatalf("OpenTraffic: %v", err)
	}
	host.releaseTraffic()
	conn, err := grpc.NewClient("passthrough:///"+s.Addr(), dialOption)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Stop(stopCtx)
		host.shutdown()
	})
	return recorder, conn
}

// handshakeSerial performs a fresh TLS handshake against addr and reports the
// serial of the certificate the server presented. A fresh connection is the
// only place a rotation can be observed: established connections keep what
// they negotiated.
func handshakeSerial(t *testing.T, addr string, config *tls.Config) (int64, error) {
	t.Helper()
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", addr, config)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		t.Fatal("handshake presented no certificate")
	}
	return state.PeerCertificates[0].SerialNumber.Int64(), nil
}

// TestTLSListenerTerminatesTheHandshakeAndServesRPCs is the acceptance path:
// with grpc.tls configured, a client that trusts the generated certificate
// speaks to the service, the handshake negotiates h2, and a plaintext client
// is refused -- which is what "termination now happens in this process" means
// on the wire.
func TestTLSListenerTerminatesTheHandshakeAndServesRPCs(t *testing.T) {
	certFile, keyFile, roots := writeTestCertificate(t)
	cfg := DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.TLS = TLSConfig{CertFile: certFile, KeyFile: keyFile}
	s := newServer(cfg, contributions{registrars: []plugin.Entry[ServiceRegistrar]{echoContributor(&echoImpl{})}})
	recorder, conn := serveRecorded(t, s, grpc.WithTransportCredentials(credentials.NewTLS(tlsClientConfig(roots))))

	out, err := callEcho(t, conn, "hi")
	if err != nil || out.GetValue() != "echo:hi" {
		t.Fatalf("Echo over TLS = %v, %v; want echo:hi", out, err)
	}

	// The handshake itself, observed beside the gRPC call: h2 negotiated and
	// a version floor at or above 1.2.
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	raw, err := tls.DialWithDialer(dialer, "tcp", s.Addr(), &tls.Config{
		RootCAs:    roots,
		ServerName: "localhost",
		NextProtos: []string{"h2"},
	})
	if err != nil {
		t.Fatalf("raw TLS handshake: %v", err)
	}
	state := raw.ConnectionState()
	raw.Close()
	if state.NegotiatedProtocol != "h2" {
		t.Fatalf("ALPN = %q, want h2", state.NegotiatedProtocol)
	}
	if state.Version < tls.VersionTLS12 {
		t.Fatalf("TLS version = %x, want >= 1.2", state.Version)
	}

	// A plaintext client cannot talk to this listener at all.
	plaintext, err := grpc.NewClient("passthrough:///"+s.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer plaintext.Close()
	if _, err := callEcho(t, plaintext, "hi"); err == nil {
		t.Fatal("a plaintext client reached a TLS listener")
	}

	if lines := strings.Join(recorder.infoLines(), "\n"); !strings.Contains(lines, "grpc: TLS termination enabled") {
		t.Fatalf("startup report does not state TLS termination:\n%s", lines)
	}
}

// TestPlaintextStaysTheDefault is the regression this feature could most
// easily break: a deployment that never writes a grpc.tls section must serve
// exactly what it served before TLS existed, and its startup report must not
// gain a line about a capability it does not use.
func TestPlaintextStaysTheDefault(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	s := newServer(cfg, contributions{registrars: []plugin.Entry[ServiceRegistrar]{echoContributor(&echoImpl{})}})
	recorder, conn := serveRecorded(t, s, grpc.WithTransportCredentials(insecure.NewCredentials()))

	if out, err := callEcho(t, conn, "hi"); err != nil || out.GetValue() != "echo:hi" {
		t.Fatalf("Echo = %v, %v; want echo:hi", out, err)
	}
	if lines := strings.Join(recorder.infoLines(), "\n"); strings.Contains(lines, "TLS") {
		t.Fatalf("a plaintext deployment's report mentions TLS:\n%s", lines)
	}
}

// TestTLSMaterialIsValidatedWithoutBinding pins that unreadable material
// fails the fallible half of a boot: Start refuses before binding or
// admitting a task, and Preflight -- the validate command -- reports the same
// failure without a listener.
func TestTLSMaterialIsValidatedWithoutBinding(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, _ := writeTestCertificate(t)
	malformedCA := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(malformedCA, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		tls   TLSConfig
		match string
	}{
		{
			name:  "missing certificate",
			tls:   TLSConfig{CertFile: filepath.Join(dir, "absent.crt"), KeyFile: filepath.Join(dir, "absent.key")},
			match: "tls.cert_file",
		},
		{
			name: "malformed client CA bundle",
			tls: TLSConfig{
				CertFile:     certFile,
				KeyFile:      keyFile,
				ClientAuth:   "require_and_verify",
				ClientCAFile: malformedCA,
			},
			match: "tls.client_ca_file",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Addr = "127.0.0.1:0"
			cfg.TLS = testCase.tls

			s := newServer(cfg, contributions{registrars: []plugin.Entry[ServiceRegistrar]{echoContributor(&echoImpl{})}})
			host := newFakeHost()
			defer host.shutdown()
			ctx := contextFromHost(host)
			err := s.Start(ctx)
			if err == nil || !strings.Contains(err.Error(), testCase.match) {
				t.Fatalf("Start error = %v, want a %q failure", err, testCase.match)
			}
			if s.Addr() != "" {
				t.Fatal("a failed Start must not leave a bound listener behind")
			}
			if host.submitted() != 0 {
				t.Fatal("a failed Start must admit no serving task")
			}
			if err := s.Stop(context.Background()); err != nil {
				t.Fatalf("Stop after failed Start = %v", err)
			}

			preflight := newServer(cfg, contributions{registrars: []plugin.Entry[ServiceRegistrar]{echoContributor(&echoImpl{})}})
			preflightHost := newFakeHost()
			defer preflightHost.shutdown()
			err = preflight.Preflight(contextFromHost(preflightHost))
			if err == nil || !strings.Contains(err.Error(), testCase.match) {
				t.Fatalf("Preflight error = %v, want a %q failure", err, testCase.match)
			}
			if preflight.Addr() != "" || preflightHost.submitted() != 0 {
				t.Fatal("Preflight activated something")
			}
		})
	}
}

// TestTLSConfigValidationRejectsInconsistentDeclarations covers the I/O-free
// half: a declaration nothing can act on is refused rather than accepted as a
// setting that quietly means something else. The certificate paths are never
// read here -- validate's contract is to stay I/O-free.
func TestTLSConfigValidationRejectsInconsistentDeclarations(t *testing.T) {
	cases := []struct {
		name  string
		tls   TLSConfig
		match string
	}{
		{name: "cert without key", tls: TLSConfig{CertFile: "cert.pem"}, match: "must be set together"},
		{name: "padded path", tls: TLSConfig{CertFile: " cert.pem", KeyFile: "key.pem"}, match: "padded with whitespace"},
		{name: "client CA without certificate", tls: TLSConfig{ClientCAFile: "ca.pem"}, match: "no TLS listener to verify clients on"},
		{name: "reload interval without certificate", tls: TLSConfig{ReloadInterval: time.Minute}, match: "no certificate to reload"},
		{name: "unsupported version", tls: TLSConfig{CertFile: "cert.pem", KeyFile: "key.pem", MinVersion: "1.1"}, match: "not supported"},
		{name: "unsupported client auth", tls: TLSConfig{CertFile: "cert.pem", KeyFile: "key.pem", ClientAuth: "request"}, match: "not supported"},
		{
			name:  "verification mode without CA",
			tls:   TLSConfig{CertFile: "cert.pem", KeyFile: "key.pem", ClientAuth: "require_and_verify"},
			match: "requires tls.client_ca_file",
		},
		{
			name:  "CA without a verification mode",
			tls:   TLSConfig{CertFile: "cert.pem", KeyFile: "key.pem", ClientCAFile: "ca.pem"},
			match: "only has a job when client certificates are verified",
		},
		{
			name:  "unknown cipher suite",
			tls:   TLSConfig{CertFile: "cert.pem", KeyFile: "key.pem", CipherSuites: []string{"TLS_RC4_128_SHA"}},
			match: "not a secure TLS cipher suite",
		},
		{
			name: "duplicate cipher suite",
			tls: TLSConfig{CertFile: "cert.pem", KeyFile: "key.pem", CipherSuites: []string{
				"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
				"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			}},
			match: "twice",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.tls.validate()
			if err == nil || !strings.Contains(err.Error(), testCase.match) {
				t.Fatalf("validate() error = %v, want a %q failure", err, testCase.match)
			}
		})
	}
}

// TestCertificateRotationIsPickedUpOnTheNextHandshake is the whole point of
// GetCertificate, observed where an operator would observe it: a deployment
// replaces the files under a running process, and the next handshake is served
// the new certificate. reload_interval 0 checks the file on every handshake,
// which is the default a rotation written by an external tool relies on.
func TestCertificateRotationIsPickedUpOnTheNextHandshake(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, certificate := writeCertificatePair(t, dir, 1)
	roots := x509.NewCertPool()
	roots.AddCert(certificate)

	cfg := DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.TLS = TLSConfig{CertFile: certFile, KeyFile: keyFile}
	s := newServer(cfg, contributions{registrars: []plugin.Entry[ServiceRegistrar]{echoContributor(&echoImpl{})}})
	recorder, _ := serveRecorded(t, s, grpc.WithTransportCredentials(credentials.NewTLS(tlsClientConfig(roots))))

	serial, err := handshakeSerial(t, s.Addr(), tlsClientConfig(roots))
	if err != nil || serial != 1 {
		t.Fatalf("handshake serial = %d, %v; want the pair present at startup", serial, err)
	}
	if lines := strings.Join(recorder.infoLines(), "\n"); strings.Contains(lines, "reloaded") {
		t.Fatal("a pair that has not moved is not a rotation, and must not be logged as one")
	}

	_, _, rotated := writeCertificatePair(t, dir, 2)
	roots.AddCert(rotated)
	serial, err = handshakeSerial(t, s.Addr(), tlsClientConfig(roots))
	if err != nil || serial != 2 {
		t.Fatalf("handshake serial after rotation = %d, %v; want 2 without a restart", serial, err)
	}
	if lines := strings.Join(recorder.infoLines(), "\n"); !strings.Contains(lines, "grpc: TLS certificate reloaded") {
		t.Fatalf("a rotation an operator cannot see in the log is a rotation they cannot audit:\n%s", lines)
	}
	if errors := recorder.errorLines(); len(errors) != 0 {
		t.Fatalf("a rotation that worked logged errors: %v", errors)
	}
}

// TestBrokenRotationKeepsServingThePreviousCertificate pins the failure
// policy: an unreadable replacement is a rotation in progress, not a reason
// to stop serving, so the previous certificate keeps answering and the
// failure is reported.
func TestBrokenRotationKeepsServingThePreviousCertificate(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, certificate := writeCertificatePair(t, dir, 1)
	roots := x509.NewCertPool()
	roots.AddCert(certificate)

	cfg := DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.TLS = TLSConfig{CertFile: certFile, KeyFile: keyFile}
	s := newServer(cfg, contributions{registrars: []plugin.Entry[ServiceRegistrar]{echoContributor(&echoImpl{})}})
	recorder, _ := serveRecorded(t, s, grpc.WithTransportCredentials(credentials.NewTLS(tlsClientConfig(roots))))

	// A half-written key: the pair's identity moves (size changes) but it no
	// longer loads.
	if err := os.WriteFile(keyFile, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(keyFile, time.Now().Add(3*time.Second), time.Now().Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}

	serial, err := handshakeSerial(t, s.Addr(), tlsClientConfig(roots))
	if err != nil || serial != 1 {
		t.Fatalf("handshake serial under a broken rotation = %d, %v; want the previous certificate to keep serving", serial, err)
	}
	if errors := strings.Join(recorder.errorLines(), "\n"); !strings.Contains(errors, "grpc: TLS certificate reload failed") {
		t.Fatalf("a refused rotation must be reported:\n%s", errors)
	}
}

// TestMissingCertificateIsReportedOnce pins the deduplication: a certificate
// that stays gone stays one report, because a report per handshake would
// drown the log. The old certificate keeps serving throughout.
func TestMissingCertificateIsReportedOnce(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, certificate := writeCertificatePair(t, dir, 1)
	roots := x509.NewCertPool()
	roots.AddCert(certificate)

	cfg := DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.TLS = TLSConfig{CertFile: certFile, KeyFile: keyFile}
	s := newServer(cfg, contributions{registrars: []plugin.Entry[ServiceRegistrar]{echoContributor(&echoImpl{})}})
	recorder, _ := serveRecorded(t, s, grpc.WithTransportCredentials(credentials.NewTLS(tlsClientConfig(roots))))

	if err := os.Remove(certFile); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		serial, err := handshakeSerial(t, s.Addr(), tlsClientConfig(roots))
		if err != nil || serial != 1 {
			t.Fatalf("handshake serial with a missing certificate = %d, %v; want 1", serial, err)
		}
	}
	if errors := recorder.errorLines(); len(errors) != 1 {
		t.Fatalf("missing-certificate reports = %d, want 1: %v", len(errors), errors)
	}
}

// TestReloadIntervalBoundsHowOftenTheFileIsChecked pins the rate limit through
// the source's own clock: the interval -- not the write -- is what decides
// whether a rotation is seen, and the first handshake after the interval is
// the one that sees it.
func TestReloadIntervalBoundsHowOftenTheFileIsChecked(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, _ := writeCertificatePair(t, dir, 1)
	recorder := &recordingLogger{}
	source, err := newCertificateSource(TLSConfig{
		CertFile:       certFile,
		KeyFile:        keyFile,
		ReloadInterval: time.Hour,
	}, recorder)
	if err != nil {
		t.Fatal(err)
	}

	clock := time.Now()
	source.now = func() time.Time { return clock }
	serialOf := func(t *testing.T) int64 {
		t.Helper()
		certificate, err := source.getCertificate(nil)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := x509.ParseCertificate(certificate.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		return parsed.SerialNumber.Int64()
	}

	if serial := serialOf(t); serial != 1 {
		t.Fatalf("serial = %d, want the pair present at construction", serial)
	}

	_, _, rotated := writeCertificatePair(t, dir, 2)
	if rotated.SerialNumber.Int64() != 2 {
		t.Fatal("test setup wrote the wrong serial")
	}

	if serial := serialOf(t); serial != 1 {
		t.Fatal("a handshake inside the claimed interval must not state the file")
	}
	if serial := serialOf(t); serial != 1 {
		t.Fatal("a handshake inside the claimed interval must not state the file")
	}
	if lines := strings.Join(recorder.infoLines(), "\n"); strings.Contains(lines, "reloaded") {
		t.Fatal("no reload may be logged inside the interval")
	}

	clock = clock.Add(time.Hour + time.Second)
	if serial := serialOf(t); serial != 2 {
		t.Fatal("the first handshake after the interval is the one that sees the rotation")
	}
	if lines := strings.Join(recorder.infoLines(), "\n"); !strings.Contains(lines, "grpc: TLS certificate reloaded") {
		t.Fatalf("the reload was not logged:\n%s", lines)
	}
}

// TestMutualTLSRequiresAVerifiedClientCertificate pins the half of grpc.tls
// that only a handshake can show: client_auth and client_ca_file reach the TLS
// configuration as a verification policy, not merely as validated strings. The
// accepted client is what makes the two refusals evidence -- a server that
// refused every client would satisfy both negative assertions.
func TestMutualTLSRequiresAVerifiedClientCertificate(t *testing.T) {
	dir := t.TempDir()
	caFile, ca, caKey := writeClientCA(t, dir, "client-ca")
	_, lookalikeCA, lookalikeKey := writeClientCA(t, t.TempDir(), "client-ca")
	certFile, keyFile, roots := writeTestCertificate(t)

	cfg := DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.TLS = TLSConfig{
		CertFile:     certFile,
		KeyFile:      keyFile,
		ClientCAFile: caFile,
		ClientAuth:   "require_and_verify",
	}
	s := newServer(cfg, contributions{registrars: []plugin.Entry[ServiceRegistrar]{echoContributor(&echoImpl{})}})

	accepted := tlsClientConfig(roots)
	accepted.Certificates = []tls.Certificate{writeClientCertificate(t, ca, caKey, 1)}
	_, conn := serveRecorded(t, s, grpc.WithTransportCredentials(credentials.NewTLS(accepted)))
	if out, err := callEcho(t, conn, "hi"); err != nil || out.GetValue() != "echo:hi" {
		t.Fatalf("Echo with a verified client certificate = %v, %v; want echo:hi", out, err)
	}

	// The refusals are probed with real fail-fast clients rather than a bare
	// handshake: under TLS 1.3 the client's handshake can complete before the
	// server's verification result arrives, so the refusal surfaces on the
	// first use of the connection, which is what these calls exercise.
	withoutCertificate := tlsClientConfig(roots)
	refused, err := grpc.NewClient("passthrough:///"+s.Addr(), grpc.WithTransportCredentials(credentials.NewTLS(withoutCertificate)))
	if err != nil {
		t.Fatal(err)
	}
	defer refused.Close()
	if _, err := callEcho(t, refused, "hi"); err == nil {
		t.Fatal("a client presenting no certificate was accepted")
	}

	untrusted := tlsClientConfig(roots)
	untrusted.Certificates = []tls.Certificate{writeClientCertificate(t, lookalikeCA, lookalikeKey, 2)}
	refused, err = grpc.NewClient("passthrough:///"+s.Addr(), grpc.WithTransportCredentials(credentials.NewTLS(untrusted)))
	if err != nil {
		t.Fatal(err)
	}
	defer refused.Close()
	if _, err := callEcho(t, refused, "hi"); err == nil {
		t.Fatal("a client certificate from an untrusted CA was accepted")
	}
}
