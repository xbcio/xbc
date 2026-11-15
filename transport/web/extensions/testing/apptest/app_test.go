package apptest

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// greeter is the smallest plugin a test can compose into the application: one
// public route with a fixed answer, so a 200 proves the request reached a
// contributed handler rather than the harness probe.
type greeter struct{}

var _ web.RouteContributor = (*greeter)(nil)

func (*greeter) RegisterRoutes(router *web.Router) {
	router.GET("/hello", hello).Name("apptest.test.hello").Auth(web.Public()).Unmetered()
}

func hello(_ context.Context, c *web.Ctx) error {
	c.JSON(http.StatusOK, map[string]string{"hello": "world"})
	return nil
}

// greeterBundle keeps the test plugin out of every graph that did not ask for
// it; only the tests passing it to WithBundles serve /hello.
var greeterBundle = plugin.BundleOf(plugin.Define(
	"apptest-test-greeter",
	func(plugin.BuildContext) (*greeter, error) { return &greeter{}, nil },
	plugin.Options[*greeter]{
		Exports: plugin.Contracts(
			plugin.ExportAs[web.RouteContributor](func(value *greeter) web.RouteContributor { return value }),
		),
	},
))

func TestNewServesComposedRoutes(t *testing.T) {
	app := New(t, WithBundles(greeterBundle))

	assertJSON(t, app, "/hello", map[string]string{"hello": "world"})
	assertJSON(t, app, readyPath, map[string]bool{"ready": true})
}

// TestNewRunsIndependentApplications pins the port reservation and cleanup
// strategy: the second application must come up on its own port while the
// first one keeps serving, and the first must still be reachable afterwards.
func TestNewRunsIndependentApplications(t *testing.T) {
	first := New(t, WithBundles(greeterBundle))
	second := New(t, WithBundles(greeterBundle))

	if first.BaseURL == second.BaseURL {
		t.Fatalf("both applications report the same base URL %s", first.BaseURL)
	}
	assertJSON(t, first, "/hello", map[string]string{"hello": "world"})
	assertJSON(t, second, "/hello", map[string]string{"hello": "world"})
	assertJSON(t, first, "/hello", map[string]string{"hello": "world"})
}

// TestLaunchFailsWhenTheApplicationCannotStart drives the early-exit path:
// the overlay makes the Web server bind an address the kernel rejects, so
// Execute returns before the probe can ever answer.
func TestLaunchFailsWhenTheApplicationCannotStart(t *testing.T) {
	_, err := launch(t, newOptions(WithYAML("web:\n  addr: 127.0.0.1:not-a-port\n")))
	if err == nil {
		t.Fatal("launch succeeded with an address the kernel cannot bind")
	}
	if !strings.Contains(err.Error(), "exit code") {
		t.Fatalf("error does not report the runtime's exit status: %v", err)
	}
}

func assertJSON[T any](t *testing.T, app *App, path string, want T) {
	t.Helper()
	response, err := app.Client.Get(app.URL(path))
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, want %d", path, response.StatusCode, http.StatusOK)
	}
	var body T
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("GET %s: decode body: %v", path, err)
	}
	got, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("GET %s: marshal body: %v", path, err)
	}
	expected, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("GET %s: marshal expectation: %v", path, err)
	}
	if string(got) != string(expected) {
		t.Fatalf("GET %s: body = %s, want %s", path, got, expected)
	}
}
