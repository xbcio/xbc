package management

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// bootHost is the smallest faithful RuntimeHost these tests need: one execution
// context, one traffic gate, and task admission -- the same three facts a real
// runtime owns. Tasks receive the execution context, so cancelling it is what
// unblocks a serving task still waiting for a gate that will never release.
type bootHost struct {
	execution context.Context
	cancel    context.CancelFunc
	gate      chan struct{}
	gateOnce  sync.Once
	mu        sync.Mutex
	submitted int
	tasks     sync.WaitGroup
	logger    log.Logger
}

var _ plugin.RuntimeHost = (*bootHost)(nil)

func newBootHost() *bootHost {
	execution, cancel := context.WithCancel(context.Background())
	return &bootHost{execution: execution, cancel: cancel, gate: make(chan struct{}), logger: log.Nop()}
}

func (h *bootHost) ExecutionContext() context.Context { return h.execution }
func (h *bootHost) Logger() log.Logger                { return h.logger }
func (*bootHost) ProcessInstance() string             { return "management-test-process" }

func (*bootHost) Service() plugin.ServiceIdentity { return plugin.ServiceIdentity{} }

func (*bootHost) IngressStopBudget() time.Duration                                  { return 0 }
func (h *bootHost) TrafficGate() <-chan struct{}                                    { return h.gate }
func (*bootHost) Admission(plugin.Identity) plugin.Admission                        { return nil }
func (*bootHost) AdmissionFor(plugin.Identity, plugin.WorkloadKey) plugin.Admission { return nil }
func (*bootHost) RequestShutdown(plugin.Identity, string) bool                      { return false }

func (h *bootHost) SubmitTask(_ plugin.Identity, fn func(context.Context), _ bool) bool {
	if fn == nil {
		return false
	}
	h.mu.Lock()
	h.submitted++
	h.mu.Unlock()
	h.tasks.Add(1)
	go func() {
		defer h.tasks.Done()
		fn(h.execution)
	}()
	return true
}

func (h *bootHost) releaseTraffic() { h.gateOnce.Do(func() { close(h.gate) }) }

func (h *bootHost) submissions() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.submitted
}

func (h *bootHost) shutdown() {
	h.cancel()
	h.tasks.Wait()
}

func (h *bootHost) contextFor(key plugin.Key) *plugin.Context {
	return plugin.NewRuntimeContext(h, plugin.Identity{Plugin: key, Instance: plugin.DefaultInstance})
}

// recordingLogger collects rendered lines so an assertion can read what a boot
// told the operator.
type recordingLogger struct {
	mu     sync.Mutex
	infos  []string
	warns  []string
	errors []string
}

var _ log.Logger = (*recordingLogger)(nil)

func (*recordingLogger) Debug(string, ...any) {}
func (l *recordingLogger) Info(msg string, _ ...any) {
	l.append(&l.infos, msg)
}
func (l *recordingLogger) Warn(msg string, _ ...any) {
	l.append(&l.warns, msg)
}
func (l *recordingLogger) Error(msg string, _ ...any) {
	l.append(&l.errors, msg)
}
func (*recordingLogger) Fatal(msg string, _ ...any) { panic("recordingLogger.Fatal: " + msg) }
func (l *recordingLogger) With(_ ...any) log.Logger { return l }
func (*recordingLogger) Enabled(log.Level) bool     { return true }

func (l *recordingLogger) append(destination *[]string, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	*destination = append(*destination, msg)
}

func (l *recordingLogger) infoLines() []string  { return l.snapshot(&l.infos) }
func (l *recordingLogger) warnLines() []string  { return l.snapshot(&l.warns) }
func (l *recordingLogger) errorLines() []string { return l.snapshot(&l.errors) }

func (l *recordingLogger) snapshot(source *[]string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), *source...)
}

// fetch performs one GET over a fresh connection and reports the status code
// and body. A client per call is deliberate: a pooled connection would let a
// keep-alive from an earlier probe decide whether a later one reaches a
// listener that has just been shut down.
func fetch(addr, path string, timeout time.Duration) (int, string, error) {
	client := &http.Client{Timeout: timeout}
	response, err := client.Get("http://" + addr + path)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, "", err
	}
	return response.StatusCode, string(body), nil
}

// pollUntil reports whether condition became true before the deadline.
func pollUntil(deadline time.Duration, interval time.Duration, condition func() bool) bool {
	limit := time.Now().Add(deadline)
	for time.Now().Before(limit) {
		if condition() {
			return true
		}
		time.Sleep(interval)
	}
	return condition()
}

// waitForServing polls until a request is answered, which is only possible
// after the runtime traffic gate has been released and the serving task reached
// Serve.
func waitForServing(t *testing.T, addr, path string) {
	t.Helper()
	if !pollUntil(2*time.Second, 20*time.Millisecond, func() bool {
		_, _, err := fetch(addr, path, 250*time.Millisecond)
		return err == nil
	}) {
		t.Fatalf("the listener on %s never started serving %s", addr, path)
	}
}

// freeAddress reserves a loopback port, releases it, and returns the address
// for a test that has to write a real one into configuration. Port 0 cannot be
// configured -- the binder validates the port like any other deployment's --
// so a planning test needs a concrete spelling.
func freeAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	return addr
}

// expositionRegistry is a real registry with one incremented counter, so an
// assertion on the rendered exposition reads a value the metric package itself
// produced.
func expositionRegistry(t *testing.T) *prometheus.Registry {
	t.Helper()
	registry := prometheus.NewRegistry()
	counter := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "management_probe_total",
		Help: "Scrapes observed by the management probe registry.",
	})
	registry.MustRegister(counter)
	counter.Inc()
	return registry
}

// blockingGatherer parks inside Gather until released, which is how a scrape is
// held in flight while Stop drains, and counts how many Gather calls were
// started.
type blockingGatherer struct {
	inner   prometheus.Gatherer
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	started atomic.Int64
}

func newBlockingGatherer(t *testing.T) *blockingGatherer {
	t.Helper()
	return &blockingGatherer{
		inner:   expositionRegistry(t),
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
}

func (g *blockingGatherer) Gather() ([]*dto.MetricFamily, error) {
	g.started.Add(1)
	select {
	case g.entered <- struct{}{}:
	default:
	}
	<-g.release
	return g.inner.Gather()
}

func (g *blockingGatherer) unblock() { g.once.Do(func() { close(g.release) }) }

// writeCertificatePair writes a self-signed certificate and its key, returning
// the two paths. The pair is what a TLS listener needs; nothing here asserts
// anything about the identity it names.
func writeCertificatePair(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a certificate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "management-test"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating a self-signed certificate: %v", err)
	}
	encodedKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("encoding the certificate key: %v", err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "management.crt")
	keyFile = filepath.Join(dir, "management.key")
	writePEM(t, certFile, "CERTIFICATE", der)
	writePEM(t, keyFile, "EC PRIVATE KEY", encodedKey)
	return certFile, keyFile
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	encoded := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
