package examples_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/transport/web"
	ginengine "github.com/xbcio/xbc/transport/web/engines/gin"
	"github.com/xbcio/xbc/transport/web/extensions/observability/pprof"
)

// probeWait bounds every wait in this file. It is generous on purpose: a
// failure should name what did not happen rather than trip on a loaded machine.
const probeWait = 15 * time.Second

// TestPprofRoutesRegisterOnlyWhenConfigured covers the route-registration half
// of the management endpoint, which nothing else can: registration receives a
// *web.Router, a value only the framework constructs while assembling a real
// server, so the extension's own package can exercise its handler but never the
// table it lands in.
//
// An enabled plugin contributes three routes -- the index at the exact
// configured path, the profile catch-all below it, and the POST form the pprof
// tool uses for symbol lookups -- and every case probes all three. One probe
// cannot see the others: the catch-all answers a trailing-slash index even when
// the exact index route is gone, and nothing but a POST reaches the command
// route, so a registration dropped from the table would otherwise fail only in
// the field.
//
// The cases differ in what registration has to get from configuration -- the
// activation section, its enabled key, and the path the routes are named under
// -- and the two that expect no endpoint run under a deny-by-default policy
// with no authenticator registered. That is the framework's own check on route
// registration: a route with no authentication policy cannot start there, so a
// plugin that registered while disabled would fail startup instead of merely
// answering 404, and the case could not pass for the wrong reason.
//
// A refused connection means "not listening yet", not "no route", so every case
// waits for a real HTTP response before asserting which one it got.
func TestPprofRoutesRegisterOnlyWhenConfigured(t *testing.T) {
	for _, tc := range []struct {
		name string
		// section is the plugins.pprof block; empty means the composition
		// configures the plugin not at all.
		section string
		// path is where the endpoint must answer, and absent is where it must
		// not: empty absent means there is no second path to check.
		path   string
		absent string
		status int
		// command is the status a POST to the symbol endpoint must produce.
		// Where the plugin registers, the command route answers 200; where it
		// does not, nothing matches the path at all and the answer is 404 --
		// the engine's 405 for a path that exists under another method belongs
		// only to the composition where a registration is missing, which is
		// what the 200 pins.
		command int
		// policy is web.security.default. A case that expects the endpoint
		// must permit, because the published routes carry no policy of their
		// own; a case that expects nothing uses deny to prove no route exists.
		policy string
	}{
		{
			name:    "section absent",
			policy:  "deny",
			path:    "/debug/pprof/",
			status:  http.StatusNotFound,
			command: http.StatusNotFound,
		},
		{
			name:    "enabled",
			policy:  "permit",
			section: "plugins:\n  pprof:\n    enabled: true\n",
			path:    "/debug/pprof/",
			status:  http.StatusOK,
			command: http.StatusOK,
		},
		{
			// The path the plugin is configured with is the path it serves:
			// the default must stop answering once another one is named.
			name:    "configured path",
			policy:  "permit",
			section: "plugins:\n  pprof:\n    enabled: true\n    path: /internal/profile\n",
			path:    "/internal/profile/",
			absent:  "/debug/pprof/",
			status:  http.StatusOK,
			command: http.StatusOK,
		},
		{
			// Presence of the section and its enabled key are two gates, not
			// one: a section that configures the plugin without turning it on
			// activates the Definition and still serves nothing. The plugin
			// checks its own enabled setting before registering, which is what
			// keeps the route out of the table the deny case would reject.
			name:    "configured path without enabled",
			policy:  "deny",
			section: "plugins:\n  pprof:\n    path: /internal/profile\n",
			path:    "/internal/profile/",
			status:  http.StatusNotFound,
			command: http.StatusNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := freeAddress(t)
			configPath := filepath.Join(t.TempDir(), "application.yml")
			config := fmt.Sprintf(`log:
  console:
    enabled: false
  file:
    enabled: false
web:
  addr: %q
  security:
    default: %s
%s`, addr, tc.policy, tc.section)
			require.NoError(t, os.WriteFile(configPath, []byte(config), 0o600))

			app, err := xbc.New(xbc.WithBundles(web.Bundle(), ginengine.Bundle(), pprof.Bundle()))
			require.NoError(t, err)

			// Execute's exit is published by closing a channel rather than by
			// sending a value, so the probe and the cleanup observe the same
			// one event: neither consumes a result the other still needs, and
			// a composition that refuses to start is reported as that rather
			// than as an endpoint that never answered.
			parent, cancel := context.WithCancel(context.Background())
			var (
				exitCode   int
				executeErr error
			)
			exited := make(chan struct{})
			go func() {
				exitCode, executeErr = app.Execute(parent, []string{"--config", configPath})
				close(exited)
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-exited:
				case <-time.After(probeWait):
					t.Error("timed out waiting for Execute cleanup")
				}
			})

			client := &http.Client{Timeout: 250 * time.Millisecond}
			t.Cleanup(client.CloseIdleConnections)
			send := func(method, path string) (*http.Response, error) {
				request, requestErr := http.NewRequest(method, "http://"+addr+path, nil)
				if requestErr != nil {
					return nil, requestErr
				}
				return client.Do(request)
			}
			// answered polls until the server produces any HTTP response at
			// all, so the assertion below compares statuses the composition
			// chose rather than the paucity of its startup.
			answered := func(method, path string) *http.Response {
				deadline := time.Now().Add(probeWait)
				for {
					response, requestErr := send(method, path)
					if requestErr == nil {
						return response
					}
					select {
					case <-exited:
						t.Fatalf("the process exited before %s %s answered: code=%d error=%v", method, path, exitCode, executeErr)
					default:
					}
					if time.Now().After(deadline) {
						t.Fatalf("timed out waiting for %s %s: %v", method, path, requestErr)
					}
					time.Sleep(10 * time.Millisecond)
				}
			}

			response := answered(http.MethodGet, tc.path)
			defer response.Body.Close()
			require.Equal(t, tc.status, response.StatusCode)
			if tc.status == http.StatusOK {
				assert.Equal(t, "no-store", response.Header.Get("Cache-Control"),
					"a profile index must not be cached")
			}

			// The server has answered once, so the remaining probes are
			// assertions rather than waits. The command route is the one the
			// pprof tool itself uses, and the index without its trailing slash
			// is the URL a person types: each pins the registration the
			// trailing-slash probe cannot reach.
			noSlash := strings.TrimSuffix(tc.path, "/")
			index, requestErr := send(http.MethodGet, noSlash)
			require.NoError(t, requestErr)
			defer index.Body.Close()
			assert.Equal(t, tc.status, index.StatusCode,
				"the index route must answer %s, the path without its trailing slash", noSlash)

			symbol, requestErr := send(http.MethodPost, tc.path+"symbol")
			require.NoError(t, requestErr)
			defer symbol.Body.Close()
			assert.Equal(t, tc.command, symbol.StatusCode,
				"the command route must answer POST %s", tc.path+"symbol")

			if tc.absent != "" {
				// The server has already answered once, so nothing here waits
				// on startup; a refusal at this path is the answer.
				missing, requestErr := send(http.MethodGet, tc.absent)
				require.NoError(t, requestErr)
				defer missing.Body.Close()
				assert.Equal(t, http.StatusNotFound, missing.StatusCode,
					"the configured path must move the routes, not add to them")
			}

			cancel()
			select {
			case <-exited:
				require.NoError(t, executeErr)
				assert.Equal(t, 0, exitCode)
			case <-time.After(probeWait):
				t.Fatal("timed out waiting for Execute after the endpoint answered")
			}
		})
	}
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
