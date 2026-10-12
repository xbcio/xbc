package accesslog

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corelog "github.com/xbcio/xbc/log"
)

// TestAccessLogLineRepeatsNoKeyWithTheJSONIdentity pins the F2 key choice on
// this module's own line: the access log writes a short "service" field (the
// RPC service name), and the process identity a JSON sink stamps is written
// under the OpenTelemetry key "service.name", so the two coexist on one line
// without sharing a key. Before the fix both used "service", and most
// collectors read a repeated key as last-one-wins -- one of the two values
// silently disappeared. This drives the interceptor through a real JSON sink
// carrying the identity and asserts every key on the line appears exactly
// once, with both values intact.
func TestAccessLogLineRepeatsNoKeyWithTheJSONIdentity(t *testing.T) {
	dir := t.TempDir()
	cfg := corelog.DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = filepath.Join(dir, "grpc.jsonl")
	cfg.Service = corelog.ServiceIdentity{Name: "orders", Version: "v1.2.3", Instance: "host-7-123"}
	if err := corelog.Init(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corelog.Close(); corelog.SetLogger(corelog.Nop()) })

	p, err := newPlugin(DefaultConfig(), corelog.L())
	if err != nil {
		t.Fatal(err)
	}
	if err := callUnary(p, peerContext(), func(context.Context, any) (any, error) { return "resp", nil }); err != nil {
		t.Fatal(err)
	}
	if err := corelog.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(cfg.File.Path)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(data))

	// Every key the line can carry, counted in its encoded `"key":` form.
	// None of these patterns is a substring of another (a quoted key is
	// followed by a colon, never by another key character), and the fixed
	// field values contain no quote-colon sequence, so the counts are exact.
	expectedKeys := []string{
		`"level":`, `"ts":`, `"caller":`, `"msg":`,
		`"service.name":`, `"service.version":`, `"service.instance.id":`,
		`"service":`, `"method":`, `"kind":`, `"code":`, `"latency":`, `"peer":`, `"panicked":`,
	}
	for _, key := range expectedKeys {
		if got := strings.Count(line, key); got != 1 {
			t.Fatalf("key %s appears %d times, want exactly once in: %s", key, got, line)
		}
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(line), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != len(expectedKeys) {
		t.Errorf("line carries %d distinct keys, want %d: %s", len(decoded), len(expectedKeys), line)
	}
	if decoded["service"] != "xbc.test.AccessLog" {
		t.Errorf("service = %#v, want the RPC service name under its own key", decoded["service"])
	}
	if decoded["service.name"] != "orders" {
		t.Errorf("service.name = %#v, want the process identity under its own key", decoded["service.name"])
	}
}
