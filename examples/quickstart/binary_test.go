//go:build unix && !aix && !solaris

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
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
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("Go command is unavailable, skip the binary smoke test")
	}

	// go test runs with the package directory as its working directory, which
	// is the quickstart module directory this build and the child process both
	// need: "." is this package, and application.yml resolves beside it.
	binary := filepath.Join(t.TempDir(), "quickstart")
	build := exec.Command(goTool, "build", "-o", binary, ".")
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("go build: %v\n%s", buildErr, output)
	}

	address := freeAddress(t)
	command := exec.Command(binary, "--config", "application.yml")
	command.Env = append(os.Environ(), "XBC_WEB_ADDR="+address)
	output := new(syncBuffer)
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		t.Fatalf("start the quickstart binary: %v", err)
	}
	// Exit is published by closing a channel rather than by sending a value,
	// so the readiness wait and the cleanup observe the same one event: a
	// process that dies during startup is reported where it happens, and the
	// cleanup can still kill and reap a process that is still running.
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

	base := "http://" + address + "/api/v1"
	waitForReadiness(t, base+"/readyz", exited, func() error { return exitErr }, output.String)
	status, body := get(t, base+"/hello", output.String)
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
	case <-exited:
		if exitErr != nil {
			t.Fatalf("the process exited with %v, want a clean exit\n%s", exitErr, output.String())
		}
	case <-time.After(startWait):
		t.Fatalf("timed out waiting for the process to stop\n%s", output.String())
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
// of both failures, because that is where a startup error explains itself.
func waitForReadiness(t *testing.T, url string, exited <-chan struct{}, exitErr func() error, output func() string) {
	t.Helper()

	client := &http.Client{Timeout: time.Second}
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

func get(t *testing.T, url string, output func() string) (int, []byte) {
	t.Helper()

	response, err := (&http.Client{Timeout: 5 * time.Second}).Get(url)
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
