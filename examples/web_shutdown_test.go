// Package examples_test verifies application-level composition paths.
package examples_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/transport/web"
	ginengine "github.com/xbcio/xbc/transport/web/engines/gin"
	"github.com/xbcio/xbc/transport/web/extensions/reliability/health"
)

// TestPublicAPIReadinessIsObservableDuringWebPreDrain proves the public
// composition path keeps a fresh external probe connection reachable after
// cancellation long enough to observe health's withdrawal response.
func TestPublicAPIReadinessIsObservableDuringWebPreDrain(t *testing.T) {
	const (
		preDrainDelay   = 500 * time.Millisecond
		shutdownTimeout = 2 * time.Second
	)

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
  shutdown:
    pre_drain_delay: %s
`, shutdownTimeout, addr, preDrainDelay)
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0o600))

	app, err := xbc.New(xbc.WithBundles(web.Bundle(), ginengine.Bundle(), health.Bundle()))
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
		Timeout: 250 * time.Millisecond,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}
	t.Cleanup(client.CloseIdleConnections)
	endpoint := "http://" + addr + "/readyz"
	status := func() (int, error) {
		request, requestErr := http.NewRequest(http.MethodGet, endpoint, nil)
		if requestErr != nil {
			return 0, requestErr
		}
		request.Close = true
		response, requestErr := client.Do(request)
		if requestErr != nil {
			return 0, requestErr
		}
		defer response.Body.Close()
		return response.StatusCode, nil
	}

	startupDeadline := time.Now().Add(5 * time.Second)
	for {
		got, requestErr := status()
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

	cancel()
	shutdownObservationDeadline := time.Now().Add(shutdownTimeout)
	for {
		got, requestErr := status()
		if requestErr != nil {
			t.Fatalf("listener closed before readiness returned 503: %v", requestErr)
		}
		switch got {
		case http.StatusServiceUnavailable:
			goto readinessObserved
		case http.StatusOK:
			// Cancellation and lifecycle propagation are asynchronous; retry on a
			// new connection until the health plugin observes it.
		default:
			t.Fatalf("shutdown readiness status = %d, want 200 or 503", got)
		}
		select {
		case result := <-done:
			finished = true
			t.Fatalf("Execute returned before readiness returned 503: code=%d error=%v", result.code, result.err)
		default:
		}
		if time.Now().After(shutdownObservationDeadline) {
			t.Fatal("timed out waiting for shutdown readiness to return 503")
		}
		time.Sleep(10 * time.Millisecond)
	}

readinessObserved:
	select {
	case result := <-done:
		finished = true
		require.NoError(t, result.err)
		assert.Equal(t, 0, result.code)
	case <-time.After(shutdownTimeout + time.Second):
		t.Fatal("timed out waiting for Execute after readiness returned 503")
	}
}
