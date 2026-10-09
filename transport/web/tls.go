package web

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xbcio/xbc/log"
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
//
// The certificate is not stored on the config. GetCertificate -- and only
// GetCertificate -- is what makes reloading possible at all: a value in
// Certificates is read once, when the config is built, while GetCertificate is
// called on every handshake and can therefore answer with a certificate that
// was rotated since. Leaving Certificates empty also avoids a subtle trap:
// crypto/tls only consults GetCertificate when Certificates is empty or the
// client sent a ServerName, so a deployment dialed by IP address would
// otherwise never see a rotation. The source is loaded eagerly here anyway --
// an unreadable certificate is a startup error, not something to discover at
// the first handshake.
func (c TLSConfig) serverTLSConfig(logger log.Logger) (*tls.Config, error) {
	if strings.TrimSpace(c.CertFile) == "" {
		return nil, nil
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
	source, err := newCertificateSource(c, logger)
	if err != nil {
		return nil, err
	}

	config := &tls.Config{
		GetCertificate: source.getCertificate,
		MinVersion:     minVersion,
		CipherSuites:   cipherSuites,
		ClientAuth:     clientAuth,
		NextProtos:     []string{"http/1.1"},
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

// fileState identifies one file by the two facts a rotation changes: the
// modification time and the size. Contents are deliberately not read to decide
// whether to reload -- hashing every certificate on every handshake would be
// the cost this check exists to avoid -- so a rotation that preserves both
// facts is invisible, which is why deployments rotate by writing a new file
// (or swapping a symlink) rather than by editing in place.
type fileState struct {
	modTime time.Time
	size    int64
}

// certificateSource serves the certificate pair and notices when it changes on
// disk. Reloading is lazy on purpose: it happens on a handshake, not on a
// background goroutine, because a goroutine would have to be owned, cancelled
// and joined by the Server for a feature whose whole value is that it has no
// moving parts. The check is at most one stat per ReloadInterval across all
// concurrent handshakes (see checkDue), and a reload failure never fails the
// handshake: the previous certificate keeps serving and the error is logged
// once per broken version of the file, so a half-written rollout does not
// produce an error per connection.
type certificateSource struct {
	certFile string
	keyFile  string
	interval time.Duration
	logger   log.Logger

	mu      sync.Mutex
	current *tls.Certificate
	state   certState
	// broken identifies the file state whose load failure has already been
	// logged, so a persistent failure is reported when it appears and not on
	// every handshake it survives. brokenSet is what makes that work for a
	// file that has gone missing: readCertState reports no state at all in
	// that case, and a zero certState on its own cannot be told from "nothing
	// has failed yet".
	broken    certState
	brokenSet bool

	// nextCheck is the earliest unix-nano timestamp the next identity check may
	// run at. It is a CAS gate rather than a timer: handshakes arrive on their
	// own schedule, and all this field does is stop each of them from stating
	// the file.
	nextCheck atomic.Int64

	// now is time.Now, injectable so the rate limit is testable without waiting.
	now func() time.Time
}

// certState is the identity of the certificate pair as a whole: a rotation of
// either file is a rotation of the pair.
type certState struct {
	cert fileState
	key  fileState
}

func newCertificateSource(c TLSConfig, logger log.Logger) (*certificateSource, error) {
	source := &certificateSource{
		certFile: c.CertFile,
		keyFile:  c.KeyFile,
		interval: c.ReloadInterval,
		logger:   log.Nop(),
		now:      time.Now,
	}
	if logger != nil {
		source.logger = logger
	}
	certificate, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("xbc: web tls.cert_file/tls.key_file: %w", err)
	}
	state, err := readCertState(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("xbc: web tls.cert_file/tls.key_file: %w", err)
	}
	source.current = &certificate
	source.state = state
	return source, nil
}

// getCertificate is the tls.Config callback. It never returns an error: the
// only error it could report is a reload failure, and failing the handshake
// would turn a rotation mistake into an outage, exactly when the old
// certificate is still perfectly good.
func (s *certificateSource) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if s.checkDue() {
		s.reload()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current, nil
}

// checkDue reports whether this handshake may check the file, claiming the
// next check slot when it may. A zero interval checks nothing: every handshake
// proceeds. The CAS is what keeps N concurrent handshakes from stating the
// file N times inside one interval.
func (s *certificateSource) checkDue() bool {
	if s.interval <= 0 {
		return true
	}
	now := s.now().UnixNano()
	for {
		next := s.nextCheck.Load()
		if now < next {
			return false
		}
		if s.nextCheck.CompareAndSwap(next, now+int64(s.interval)) {
			return true
		}
	}
}

// reload swaps in the pair on disk when its identity has changed. Every
// failure path leaves the current certificate in place: an unreadable or
// half-written file is a rotation in progress, not a reason to stop serving.
func (s *certificateSource) reload() {
	state, err := readCertState(s.certFile, s.keyFile)
	if err != nil {
		s.reportBroken(state, err)
		return
	}
	s.mu.Lock()
	unchanged := state == s.state
	s.mu.Unlock()
	if unchanged {
		return
	}

	certificate, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		s.reportBroken(state, err)
		return
	}
	s.mu.Lock()
	s.current = &certificate
	s.state = state
	s.broken = certState{}
	s.brokenSet = false
	s.mu.Unlock()
	s.logger.Info("web: TLS certificate reloaded", "cert_file", s.certFile)
}

// reportBroken logs a load failure once per distinct broken file state. The
// identity of the broken file is what makes "once" meaningful: re-reporting
// the same unmoved file on every handshake would drown the log, while a new
// failure after a new write is a new fact an operator has to see. A missing
// file is an identity too -- the zero state -- and it stays one report for as
// long as the file stays gone.
func (s *certificateSource) reportBroken(state certState, err error) {
	s.mu.Lock()
	alreadyReported := s.brokenSet && state == s.broken
	s.broken = state
	s.brokenSet = true
	s.mu.Unlock()
	if alreadyReported {
		return
	}
	s.logger.Error(
		"web: TLS certificate reload failed; the previous certificate is still serving",
		"cert_file", s.certFile, "error", err,
	)
}

// readCertState stats both files of the pair.
func readCertState(certFile, keyFile string) (certState, error) {
	cert, err := os.Stat(certFile)
	if err != nil {
		return certState{}, err
	}
	key, err := os.Stat(keyFile)
	if err != nil {
		return certState{}, err
	}
	return certState{
		cert: fileState{modTime: cert.ModTime(), size: cert.Size()},
		key:  fileState{modTime: key.ModTime(), size: key.Size()},
	}, nil
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
		"web: TLS termination enabled (min_version %s, client_auth %s, cipher_suites %s, reload_interval %s)",
		cfg.MinVersion,
		cfg.ClientAuth,
		renderCipherSuites(cfg.CipherSuites),
		cfg.ReloadInterval,
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
