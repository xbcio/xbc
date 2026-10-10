// Package examples_test verifies application-level composition paths.
package examples_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	ginengine "github.com/xbcio/xbc/transport/web/engines/gin"
	"github.com/xbcio/xbc/transport/web/extensions/reliability/health"
)

// TestHealthProbesStayReachableFromASaturatedProcess is the composition-level
// regression for the exemption that lives in the health plugin's own
// registration: the probes declare themselves Unmetered, and the only way to
// see that from outside the health module is to saturate a real process and ask
// it. The mechanism itself has package-internal tests in transport/web; what
// they cannot cover is the line in transport/web/extensions/reliability/health
// that marks the two probe routes, because a *Router cannot be built from
// outside the web package.
//
// The stakes are the reason this is worth a full boot. The in-flight gate
// answers a refused request 503 before any handler runs, and a liveness probe
// reads 503 as "dead": without the exemption a process at its ceiling fails its
// own probe and is restarted exactly while it is carrying the full traffic it
// was sized for, moving that traffic onto replicas that then fail the same way.
// The blocked route is the control -- a metered route must still be refused,
// which is what makes the probe's answer a real exemption rather than a gate
// that was not holding anyone.
func TestHealthProbesStayReachableFromASaturatedProcess(t *testing.T) {
	const shutdownTimeout = 2 * time.Second

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	configPath := filepath.Join(t.TempDir(), "application.yml")
	config := fmt.Sprintf(`xbc:
  shutdown_timeout: %s
log:
  console:
    enabled: false
  file:
    enabled: false
web:
  addr: %q
  max_in_flight: 1
`, shutdownTimeout, addr)
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0o600))

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHolder := func() { releaseOnce.Do(func() { close(release) }) }
	holder := &holdingPlugin{entered: entered, release: release}
	holding := plugin.Define("holding", func(plugin.BuildContext) (*holdingPlugin, error) {
		return holder, nil
	}, plugin.Options[*holdingPlugin]{
		Exports: plugin.Contracts(
			plugin.ExportAs[web.RouteContributor](func(value *holdingPlugin) web.RouteContributor { return value }),
		),
	})

	app, err := xbc.New(xbc.WithBundles(
		web.Bundle(), ginengine.Bundle(), health.Bundle(), plugin.BundleOf(holding)))
	require.NoError(t, err)

	type executeResult struct {
		code int
		err  error
	}
	parent, cancel := context.WithCancel(context.Background())
	done := make(chan executeResult, 1)
	go func() {
		code, executeErr := app.Execute(parent, []string{"--config", configPath})
		done <- executeResult{code: code, err: executeErr}
	}()
	finished := false
	t.Cleanup(func() {
		releaseHolder()
		cancel()
		if finished {
			return
		}
		select {
		case result := <-done:
			if result.err != nil {
				t.Errorf("Execute cleanup result: code=%d error=%v", result.code, result.err)
			}
		case <-time.After(shutdownTimeout + time.Second):
			t.Error("timed out waiting for Execute cleanup")
		}
	})

	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	t.Cleanup(client.CloseIdleConnections)
	base := "http://" + addr
	get := func(path string) (int, error) {
		request, requestErr := http.NewRequest(http.MethodGet, base+path, nil)
		if requestErr != nil {
			return 0, requestErr
		}
		response, requestErr := client.Do(request)
		if requestErr != nil {
			return 0, requestErr
		}
		defer response.Body.Close()
		return response.StatusCode, nil
	}

	// Readiness answering is the signal that the listener is admitting
	// traffic; the probe itself is the first caller of the exemption.
	startupDeadline := time.Now().Add(5 * time.Second)
	for {
		got, requestErr := get("/readyz")
		if requestErr == nil {
			require.Equal(t, http.StatusOK, got, "startup readiness status")
			break
		}
		select {
		case result := <-done:
			finished = true
			t.Fatalf("Execute returned before readiness: code=%d error=%v", result.code, result.err)
		default:
		}
		if time.Now().After(startupDeadline) {
			t.Fatalf("timed out waiting for startup readiness: %v", requestErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// One request takes the whole ceiling and holds it. Its own entry into the
	// handler is what makes the saturation a fact rather than a hope.
	held := make(chan int, 1)
	go func() {
		code, _ := get("/hold")
		held <- code
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the holding request never reached its handler")
	}

	refused, err := get("/hold")
	require.NoError(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, refused,
		"a metered route is refused while the ceiling is full, so the process is genuinely saturated")

	for _, probe := range []string{"/healthz", "/readyz"} {
		code, probeErr := get(probe)
		require.NoError(t, probeErr)
		assert.Equal(t, http.StatusOK, code,
			"%s must be answered from a saturated process, or an orchestrator reads it as a dead process", probe)
	}

	releaseHolder()
	select {
	case code := <-held:
		assert.Equal(t, http.StatusOK, code, "the request that held the ceiling completes once released")
	case <-time.After(shutdownTimeout):
		t.Fatal("the holding request did not finish")
	}

	cancel()
	select {
	case result := <-done:
		finished = true
		require.NoError(t, result.err)
		assert.Equal(t, 0, result.code)
	case <-time.After(shutdownTimeout + time.Second):
		t.Fatal("timed out waiting for Execute to return")
	}
}

// holdingPlugin contributes one route that occupies the in-flight ceiling until
// the test releases it, so "the process is at its limit" is a state the test
// controls rather than one it has to catch.
type holdingPlugin struct {
	entered chan struct{}
	release chan struct{}
}

var _ web.RouteContributor = (*holdingPlugin)(nil)

func (p *holdingPlugin) RegisterRoutes(router *web.Router) {
	router.GET("/hold", p.hold).Name("holding.hold").Auth(web.Public())
}

func (p *holdingPlugin) hold(ctx context.Context, c *web.Ctx) error {
	select {
	case <-p.entered:
	default:
		close(p.entered)
	}
	select {
	case <-p.release:
		c.JSON(http.StatusOK, map[string]bool{"held": true})
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
