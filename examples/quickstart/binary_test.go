//go:build unix && !aix && !solaris

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// startWait bounds every wait in this file. It is generous on purpose: the
// first assertion a failure should produce is the one naming what did not
// happen, not a timeout on a shared machine.
const startWait = 15 * time.Second

// TestQuickstartBinaryServesAndStopsCleanly is the module's only end-to-end
// coverage of the example as it ships. Everything else reaches the same plugin
// through xbc.New and App.Execute, which is what a library consumer does; a
// process adds what only the committed command can show -- xbc.Run's ownership
// of os.Args, signals and the exit code, the committed application.yml as the
// configuration a reader would copy, and the Bundle list this package
// declares.
//
// The address is the one setting the test overrides, through the environment
// layer application.yml itself documents, because a test cannot occupy the
// port the file names. Everything else comes from the file: a configuration
// that stopped parsing, a plugin this composition no longer selects, or a
// route that stopped being public under the default deny policy all fail here.
//
// Signals are not portable to every platform this module builds on, so the
// file carries a unix build constraint rather than skipping at run time.
func TestQuickstartBinaryServesAndStopsCleanly(t *testing.T) {
	binary := buildQuickstart(t)
	address := freeAddress(t)
	process := startQuickstart(t, binary, "XBC_WEB_ADDR="+address)
	command := process.command
	output := process.output

	base := "http://" + address + "/api/v1"
	client := &http.Client{Timeout: time.Second}
	waitForReadiness(t, client, base+"/readyz", process.exited, process.exitErr, output.String)
	status, body := get(t, client, base+"/hello", output.String)
	if status != http.StatusOK {
		t.Fatalf("GET /hello = %d (%s), want 200", status, body)
	}
	var envelope struct {
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode the greeting %s: %v", body, err)
	}
	if envelope.Data.Message != "hello from xbc" {
		t.Errorf("GET /hello = %q, want the greeting the example documents", envelope.Data.Message)
	}

	// The example's own subcommand is exercised through the committed binary,
	// because what it demonstrates is a property of the command line rather
	// than of its body: the shared --config precedes the name, and everything
	// after the name reaches the command. The environment spells the address
	// the way application.yml does -- a bare port -- so the healthy run also
	// covers turning a bind address into a dial address. Both outcomes are
	// asserted, because a command that returned nil whatever it found would
	// pass the healthy half alone.
	probeEnv := append(os.Environ(), "XBC_WEB_ADDR="+portOnly(address))
	healthy := exec.Command(binary, "--config", "application.yml", "probe")
	healthy.Env = probeEnv
	probeOutput, err := healthy.CombinedOutput()
	if err != nil {
		t.Fatalf("probe against the running instance: %v\n%s", err, probeOutput)
	}
	if !strings.Contains(string(probeOutput), "answered 200") {
		t.Errorf("probe printed %q, want the endpoint it reached and its status", probeOutput)
	}

	misused := exec.Command(binary, "probe", "--config", "application.yml")
	misused.Env = probeEnv
	misuseOutput, err := misused.CombinedOutput()
	var exitErr2 *exec.ExitError
	if !errors.As(err, &exitErr2) || exitErr2.ExitCode() != 1 {
		t.Fatalf("a shared flag after the command name exited %v, want 1\n%s", err, misuseOutput)
	}
	if !strings.Contains(string(misuseOutput), "precede the command name") {
		t.Errorf("probe misuse printed %q, want the rule it broke", misuseOutput)
	}

	// A signal is how an operator stops this process, so it is what the test
	// sends: a clean exit here means the shutdown path -- traffic drain, the
	// greeter's background follow-up, plugin stop -- completed inside the
	// budgets the file configures.
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal the quickstart binary: %v", err)
	}
	select {
	case <-process.exited:
		if err := process.exitErr(); err != nil {
			t.Fatalf("the process exited with %v, want a clean exit\n%s", err, output.String())
		}
	case <-time.After(startWait):
		t.Fatalf("timed out waiting for the process to stop\n%s", output.String())
	}
}

// buildQuickstart compiles the committed example. go test runs with the
// package directory as its working directory, which is the quickstart module
// directory this build and the child process both need: "." is this package,
// and application.yml resolves beside it.
func buildQuickstart(t *testing.T) string {
	t.Helper()

	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("Go command is unavailable, skip the binary smoke test")
	}
	binary := filepath.Join(t.TempDir(), "quickstart")
	build := exec.Command(goTool, "build", "-o", binary, ".")
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("go build: %v\n%s", buildErr, output)
	}
	return binary
}

// quickstartProcess is one running instance of the built binary, plus the two
// facts every assertion about its lifetime needs: the output it logged, and the
// single event that says it stopped.
type quickstartProcess struct {
	command *exec.Cmd
	output  *syncBuffer

	// exited is closed once the process has been reaped, and exitErr then
	// holds the result. Exit is published this way rather than as a value on a
	// channel so the readiness wait and the cleanup observe the same one event:
	// a process that dies during startup is reported where it happens, and the
	// cleanup can still kill and reap a process that is still running.
	exited  <-chan struct{}
	exitErr func() error
}

func startQuickstart(t *testing.T, binary string, env ...string) *quickstartProcess {
	t.Helper()

	command := exec.Command(binary, "--config", "application.yml")
	command.Env = append(os.Environ(), env...)
	output := new(syncBuffer)
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		t.Fatalf("start the quickstart binary: %v", err)
	}
	var exitErr error
	exited := make(chan struct{})
	go func() {
		exitErr = command.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		select {
		case <-exited:
			return
		default:
		}
		_ = command.Process.Kill()
		<-exited
	})
	return &quickstartProcess{
		command: command,
		output:  output,
		exited:  exited,
		exitErr: func() error { return exitErr },
	}
}

// TestQuickstartBinaryTerminatesTLSFromTheEnvironment is the end-to-end proof
// that the two new configuration faces meet where an operator meets them: the
// environment layer binds two file paths into web.tls, the serving listener
// terminates the handshake with that material, and securityheaders -- which the
// committed application.yml selects with hsts_only_https, so it emits HSTS only
// for a request whose Request().TLS is set -- answers on the wire with the
// policy the file configures. Nothing below the wire is written by hand: a unit
// test can set Request().TLS itself (TestTLSListenerTerminatesTheHandshake does
// exactly that), and what only this process can show is that no test wrote it.
//
// The plaintext half of the protocol matters as much: a port that serves a
// certificate must not also serve an ordinary request, or a deployment that
// believes it is terminating TLS is reachable without it.
func TestQuickstartBinaryTerminatesTLSFromTheEnvironment(t *testing.T) {
	binary := buildQuickstart(t)
	certFile, keyFile, roots := writeSelfSignedPair(t)
	address := freeAddress(t)
	process := startQuickstart(t, binary,
		"XBC_WEB_ADDR="+address,
		"XBC_WEB_TLS_CERT_FILE="+certFile,
		"XBC_WEB_TLS_KEY_FILE="+keyFile,
	)

	client := tlsClient(roots)
	base := "https://" + address + "/api/v1"
	waitForReadiness(t, client, base+"/readyz", process.exited, process.exitErr, process.output.String)

	response, err := client.Get(base + "/hello")
	if err != nil {
		t.Fatalf("GET /hello over TLS: %v\n%s", err, process.output.String())
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /hello over TLS = %d, want 200\n%s", response.StatusCode, process.output.String())
	}
	if got := response.Header.Get("Strict-Transport-Security"); got != "max-age=31536000; includeSubDomains" {
		t.Errorf("Strict-Transport-Security = %q, want the policy application.yml configures; the header is emitted only for a request the middleware sees as TLS, so this is the end of the chain that starts at XBC_WEB_TLS_CERT_FILE", got)
	}

	if plaintext, err := (&http.Client{Timeout: startWait}).Get("http://" + address + "/api/v1/hello"); err == nil {
		defer plaintext.Body.Close()
		if plaintext.StatusCode == http.StatusOK {
			t.Errorf("the TLS listener served a plaintext request with 200, want a refusal\n%s", process.output.String())
		}
	}

	// The environment layer is the only place these two paths are written, so a
	// report that names TLS termination is what proves the binding reached the
	// configuration face rather than being ignored as an unknown key.
	if !strings.Contains(process.output.String(), "web: TLS termination enabled") {
		t.Errorf("startup output does not report TLS termination, so the environment layer did not reach web.tls:\n%s", process.output.String())
	}
}

// writeSelfSignedPair generates a certificate and its key in-process and returns
// their paths plus the pool a client has to trust to verify them. Reaching a
// certificate authority on the network would make this test a test of the
// network; the address the client dials is 127.0.0.1, so that is the name the
// certificate carries.
func writeSelfSignedPair(t *testing.T) (certFile, keyFile string, roots *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "quickstart"},
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
		t.Fatalf("create a certificate: %v", err)
	}
	encodedKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("encode the key: %v", err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write the certificate: %v", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encodedKey}), 0o600); err != nil {
		t.Fatalf("write the key: %v", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse the certificate: %v", err)
	}
	roots = x509.NewCertPool()
	roots.AddCert(certificate)
	return certFile, keyFile, roots
}

func tlsClient(roots *x509.CertPool) *http.Client {
	return &http.Client{
		Timeout:   startWait,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}},
	}
}

// syncBuffer collects the child process's output. exec.Cmd copies stdout and
// stderr on goroutines of its own, and the assertions below read what the
// process logged while it is still running, so a plain bytes.Buffer here would
// be a data race the detector rightly fails.
type syncBuffer struct {
	mutex sync.Mutex
	text  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.text.Write(p)
}

func (b *syncBuffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.text.String()
}

// waitForReadiness polls url until it answers 200, failing if the process
// returns first or the deadline passes. The output the process logged is part
// of both failures, because that is where a startup error explains itself. The
// client is the caller's, because whether the process serves plaintext or TLS
// is one of the things these tests choose.
func waitForReadiness(t *testing.T, client *http.Client, url string, exited <-chan struct{}, exitErr func() error, output func() string) {
	t.Helper()

	deadline := time.Now().Add(startWait)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			t.Fatalf("the process exited before becoming ready: %v\n%s", exitErr(), output())
		default:
		}
		response, err := client.Get(url)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s\n%s", url, output())
}

func get(t *testing.T, client *http.Client, url string, output func() string) (int, []byte) {
	t.Helper()

	response, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v\n%s", url, err, output())
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("GET %s: read body: %v", url, err)
	}
	return response.StatusCode, payload
}

// portOnly renders an address the way application.yml spells this example's
// bind address, as a bare ":port", so the probe child proves that a bind
// address is turned into a dial address rather than being dialed as written.
func portOnly(address string) string {
	return address[strings.LastIndex(address, ":"):]
}

func freeAddress(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return address
}
