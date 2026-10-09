package greeter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/transport/web"
	ginengine "github.com/xbcio/xbc/transport/web/engines/gin"
	"github.com/xbcio/xbc/transport/web/extensions/response/biz"
	"github.com/xbcio/xbc/transport/web/prelude"
)

// startWait bounds every wait in this file. It is generous on purpose: the
// first assertion a failure should produce is the one naming what did not
// happen, not a timeout on a shared machine.
const startWait = 15 * time.Second

// TestGreeterServesItsDocumentedContract boots the plugin in the smallest
// composition that can serve it -- the Web baseline, an engine, the business
// response envelope, and the plugin's own Bundle -- and asserts what it
// contributes: the default greeting, a greeting built from the submitted name,
// the binding failure that name is validated under, and the health check that
// registers the plugin with the readiness aggregate.
//
// The composition is deliberately not the quickstart application's. This test
// is about the plugin, so it selects what the plugin needs and leaves out what
// the example adds around it; the example's own list is exercised by running
// the example.
func TestGreeterServesItsDocumentedContract(t *testing.T) {
	address := freeAddress(t)
	configPath := filepath.Join(t.TempDir(), "application.yml")
	config := fmt.Sprintf(`log:
  console:
    enabled: false
  file:
    enabled: false
web:
  addr: %q
  base_path: /api/v1
  # Both routes this plugin contributes declare Auth(web.Public()) themselves,
  # so a deny-by-default policy stays in place and the process still starts
  # without an authenticator: nothing here reaches the network anonymously by
  # falling through to the default.
  security:
    default: deny
`, address)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write test configuration: %v", err)
	}

	app, err := xbc.New(xbc.WithBundles(
		prelude.Bundle(),
		ginengine.Bundle(),
		biz.Bundle(),
		Bundle(),
	))
	if err != nil {
		t.Fatalf("xbc.New() error = %v", err)
	}

	// Execute's exit is published by closing a channel rather than by sending a
	// value, so every waiter observes the same one event: the assertions below
	// and the cleanup can each select on it without consuming a result the
	// other still needs, and a process that fails during startup is reported
	// where it happens instead of as a timeout.
	parent, cancel := context.WithCancel(context.Background())
	var executeErr error
	exited := make(chan struct{})
	go func() {
		_, executeErr = app.Execute(parent, []string{"--config", configPath})
		close(exited)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(startWait):
			t.Error("timed out waiting for Execute cleanup")
		}
	})

	base := "http://" + address + "/api/v1"
	if waitForReadiness(t, base+"/readyz", exited) {
		t.Fatalf("the process exited before becoming ready: %v", executeErr)
	}
	client := &http.Client{Timeout: 5 * time.Second}

	status, body := call(t, client, http.MethodGet, base+"/hello", "")
	if status != http.StatusOK {
		t.Fatalf("GET /hello = %d (%s), want 200", status, body)
	}
	if greeting := decodeGreeting(t, body); greeting != "hello from xbc" {
		t.Errorf("GET /hello = %q, want the documented default greeting", greeting)
	}

	status, body = call(t, client, http.MethodPost, base+"/hello", `{"name":"XBC"}`)
	if status != http.StatusOK {
		t.Fatalf("POST /hello = %d (%s), want 200", status, body)
	}
	if greeting := decodeGreeting(t, body); greeting != "hello XBC from xbc" {
		t.Errorf("POST /hello = %q, want the greeting built from the submitted name", greeting)
	}

	// A name shorter than the struct tag's min is refused while binding, so the
	// handler never runs and the answer is a Problem Detail rather than the
	// success envelope.
	status, body = call(t, client, http.MethodPost, base+"/hello", `{"name":"X"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("POST /hello with a one-character name = %d (%s), want 400", status, body)
	}
	var problem web.ProblemDetail
	if err := json.Unmarshal(body, &problem); err != nil {
		t.Fatalf("decode the validation failure %s: %v", body, err)
	}
	if problem.Status != http.StatusBadRequest {
		t.Errorf("validation failure status = %d, want 400 (%s)", problem.Status, body)
	}

	// Registering the health check is what makes the plugin visible to
	// readiness at all, so its absence would leave this probe green; the name
	// is what proves the plugin's own check is part of the aggregate. The
	// probe reports the aggregate rather than this plugin, which is why the
	// assertion is on the check list and not on the status alone.
	status, body = call(t, client, http.MethodGet, base+"/readyz", "")
	if status != http.StatusOK {
		t.Fatalf("GET /readyz = %d (%s), want 200", status, body)
	}
	var readiness struct {
		Status string `json:"status"`
		Checks []struct {
			Name string `json:"name"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(body, &readiness); err != nil {
		t.Fatalf("decode the readiness probe %s: %v", body, err)
	}
	names := make([]string, 0, len(readiness.Checks))
	for _, check := range readiness.Checks {
		names = append(names, check.Name)
	}
	if !slices.Contains(names, Key.String()+"/greeting") {
		t.Errorf("readiness checks = %v, want the plugin's own check among them (%s)", names, body)
	}

	cancel()
	select {
	case <-exited:
		if executeErr != nil {
			t.Fatalf("shutdown returned error %v, want a clean exit", executeErr)
		}
	case <-time.After(startWait):
		t.Fatal("timed out waiting for a clean shutdown")
	}
}

// greetingEnvelope is the subset of biz.Response[Greeting] this test reads.
type greetingEnvelope struct {
	Success bool     `json:"success"`
	Code    string   `json:"code"`
	Data    Greeting `json:"data"`
}

// decodeGreeting reads one success envelope and asserts its invariants, so a
// message comparison cannot pass on a body that is not the documented shape.
func decodeGreeting(t *testing.T, body []byte) string {
	t.Helper()

	var envelope greetingEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode the success envelope %s: %v", body, err)
	}
	if !envelope.Success || envelope.Code != biz.SuccessCode {
		t.Fatalf("success envelope %s, want success=%t code=%q", body, true, biz.SuccessCode)
	}
	return envelope.Data.Message
}

func call(t *testing.T, client *http.Client, method, url, body string) (int, []byte) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, url, err)
	}
	return response.StatusCode, payload
}

// waitForReadiness polls url until it answers 200, and reports whether the
// process returned instead. A process that dies before its probe is ready is
// the failure this helper must not hide behind a timeout.
func waitForReadiness(t *testing.T, url string, exited <-chan struct{}) bool {
	t.Helper()

	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(startWait)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return true
		default:
		}
		response, err := client.Get(url)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return false
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", url)
	return false
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
