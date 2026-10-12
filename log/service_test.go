package log

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestJSONSinkStampsTheServiceIdentityOnEveryLine pins the P1 logging rule for
// the machine-readable half: every record a JSON sink writes carries the
// process identity as three stable fields, so an operator can select one
// deployment's lines out of a shared collector. The identity comes before
// whatever fields the call site attached, so it reads as a stable block
// whatever the entry contains.
func TestJSONSinkStampsTheServiceIdentityOnEveryLine(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = filepath.Join(dir, "app.jsonl")
	cfg.Service = ServiceIdentity{Name: "orders", Version: "v1.2.3", Instance: "host-7-123"}
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { _ = Close(); SetLogger(Nop()) })

	L().Info("first line")
	L().Info("second line", "order_id", 1001)
	require.NoError(t, Close())

	data, err := os.ReadFile(cfg.File.Path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 2)
	for _, line := range lines {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m))
		assert.Equal(t, "orders", m["service.name"])
		assert.Equal(t, "v1.2.3", m["service.version"])
		assert.Equal(t, "host-7-123", m["service.instance.id"])
	}
	assert.Less(t, strings.Index(lines[1], `"service.name"`), strings.Index(lines[1], `"order_id"`),
		"the identity comes before the fields the call site attached")
}

// TestJSONSinkNeverRepeatsAKeyIncludingTheIdentity pins the F2 key choice: the
// identity's three fields are the OpenTelemetry resource keys, so they cannot
// collide with the short keys a call site writes for its own purposes -- the
// named-instance logger's "instance", an access log's "service". A collision
// would put the same key on one line twice, and most collectors read that as
// last-one-wins, silently swapping the business value for the identity or the
// other way around. This drives exactly the two collision shapes that
// motivated the change and asserts the whole line has no repeated key.
func TestJSONSinkNeverRepeatsAKeyIncludingTheIdentity(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = filepath.Join(dir, "app.jsonl")
	cfg.Service = ServiceIdentity{Name: "orders", Version: "v1.2.3", Instance: "host-7-123"}
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { _ = Close(); SetLogger(Nop()) })

	L().With("instance", "cache").Info("named instance line", "service", "greeter.v1.Greeter")
	require.NoError(t, Close())

	data, err := os.ReadFile(cfg.File.Path)
	require.NoError(t, err)
	line := strings.TrimSpace(string(data))

	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(line), &m))
	assert.Equal(t, "host-7-123", m["service.instance.id"],
		"the identity keeps its own key")
	assert.Equal(t, "cache", m["instance"],
		"the call site's own field survives under its own key")
	assert.Equal(t, "greeter.v1.Greeter", m["service"],
		"the call site's own field survives under its own key")

	assert.Empty(t, repeatedJSONKeys(t, line),
		"a JSON line with a repeated key is read as last-one-wins by most collectors")
}

// repeatedJSONKeys returns every top-level key that appears more than once in
// one line of zap's JSON output. Every zap field is a top-level key, and JSON
// object grammar alternates key then value, so a string token observed while
// the root object expects a key is a key; composite values are skipped
// wholesale by following their brackets.
func repeatedJSONKeys(t *testing.T, line string) []string {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(line))
	counts := map[string]int{}
	expectKey := false
	depth := 0
	skipUntil := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
				if depth == 1 {
					expectKey = true
				} else if depth == 2 && !expectKey {
					skipUntil = 1 // a composite value of the root object
				}
			case '}', ']':
				depth--
				if depth == skipUntil {
					skipUntil = 0
					expectKey = true
				}
			}
			continue
		}
		if skipUntil > 0 || depth != 1 {
			continue
		}
		if expectKey {
			counts[token.(string)]++
			expectKey = false
		} else {
			expectKey = true
		}
	}
	var repeated []string
	for key, count := range counts {
		if count > 1 {
			repeated = append(repeated, key)
		}
	}
	sort.Strings(repeated)
	return repeated
}

// TestConsoleSinkLeavesTheServiceIdentityOut pins the other half of the
// per-sink rule: a console-format sink is read by a human looking at one
// process, where three repeated fields per line are noise, so it must not
// carry them. The split follows the format, not the sink: this file sink is
// console-formatted and still omits the identity, exactly as stdout does.
func TestConsoleSinkLeavesTheServiceIdentityOut(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = filepath.Join(dir, "app.log")
	cfg.File.Format = FormatConsole
	cfg.Service = ServiceIdentity{Name: "orders", Version: "v1.2.3", Instance: "host-7-123"}
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { _ = Close(); SetLogger(Nop()) })

	L().Info("human line")
	require.NoError(t, Close())

	data, err := os.ReadFile(cfg.File.Path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "human line")
	assert.NotContains(t, string(data), "orders")
	assert.NotContains(t, string(data), "v1.2.3")
	assert.NotContains(t, string(data), "host-7-123")
}

// TestJSONSinkWithoutIdentityStaysUnchanged pins the compatibility promise of
// the zero identity: a library embedding or a test that never fills it gets
// byte-for-byte the JSON it always got, with no fields that would otherwise
// claim an unnamed service.
func TestJSONSinkWithoutIdentityStaysUnchanged(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = filepath.Join(dir, "app.jsonl")
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { _ = Close(); SetLogger(Nop()) })

	L().Info("unidentified line")
	require.NoError(t, Close())

	data, err := os.ReadFile(cfg.File.Path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), `"service.name"`)
	assert.NotContains(t, string(data), `"service.version"`)
	assert.NotContains(t, string(data), `"service.instance.id"`)
}
