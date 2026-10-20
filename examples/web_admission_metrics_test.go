package examples_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	ginengine "github.com/xbcio/xbc/transport/web/engines/gin"
	"github.com/xbcio/xbc/transport/web/extensions/observability/metrics"
)

// TestAdmissionMetricsRemainVisibleFromASaturatedProcess is the composition-level
// proof that the admission gate's readings reach a real scrape. The unit tests
// pin the collector against a stub; what they cannot cover is the wiring that
// gives the collector a source in the first place -- the Web Definition's
// exported contract, the collector Definition's typed input, and the shared
// neutral registry -- or the deliberate placement of the scrape endpoint on the
// management listener, which carries no admission gate and therefore answers
// while the serving port refuses traffic.
//
// The process is driven to its exact ceiling: one request holds the only
// admission slot, and a second metered request is refused before any handler
// runs. A scrape at that instant must show the ceiling, the occupancy, and the
// rejection count together; a scrape after the holder releases must show the
// occupancy back at zero while the rejection count stays. Those two readings
// are what distinguish a working gate from a ceiling set too low, and they are
// the only evidence a saturated process offers, because a refused request never
// reaches the request instruments.
func TestAdmissionMetricsRemainVisibleFromASaturatedProcess(t *testing.T) {
	const (
		shutdownTimeout = 2 * time.Second
		admissionWait   = 5 * time.Second
	)

	servingAddr := freeAddress(t)
	managementAddr := freeAddress(t)

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
  management:
    addr: %q
plugins:
  metrics:
    http:
      enabled: true
`, shutdownTimeout, servingAddr, managementAddr)
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
		web.Bundle(), ginengine.Bundle(), metrics.Bundle(), plugin.BundleOf(holding)))
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

	type fetched struct {
		status int
		body   string
	}
	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	t.Cleanup(client.CloseIdleConnections)
	fetch := func(url string) (fetched, error) {
		response, requestErr := client.Get(url)
		if requestErr != nil {
			return fetched{}, requestErr
		}
		defer response.Body.Close()
		body, readErr := io.ReadAll(response.Body)
		if readErr != nil {
			return fetched{}, readErr
		}
		return fetched{status: response.StatusCode, body: string(body)}, nil
	}

	// Any HTTP response from the serving listener means it is admitting
	// traffic; the status of the unmatched path is not the subject here.
	startupDeadline := time.Now().Add(admissionWait)
	for {
		_, requestErr := fetch("http://" + servingAddr + "/")
		if requestErr == nil {
			break
		}
		select {
		case result := <-done:
			finished = true
			t.Fatalf("Execute returned before the serving listener answered: code=%d error=%v", result.code, result.err)
		default:
		}
		if time.Now().After(startupDeadline) {
			t.Fatalf("timed out waiting for the serving listener: %v", requestErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// expositionValue reads one exact series line. Exact name equality, rather
	// than a prefix match, keeps xbc_http_admission_in_flight from also
	// matching a longer name that happens to share it.
	expositionValue := func(body, name string) (string, bool) {
		for _, line := range strings.Split(body, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == name {
				return fields[1], true
			}
		}
		return "", false
	}
	awaitAdmission := func(t *testing.T, wantLimit, wantInFlight, wantRejections string) string {
		t.Helper()
		deadline := time.Now().Add(admissionWait)
		var last, limit, inFlight, rejections string
		for {
			response, requestErr := fetch("http://" + managementAddr + "/metrics")
			if requestErr == nil && response.status == http.StatusOK {
				last = response.body
				var haveLimit, haveInFlight, haveRejections bool
				limit, haveLimit = expositionValue(last, "xbc_http_admission_limit")
				inFlight, haveInFlight = expositionValue(last, "xbc_http_admission_in_flight")
				rejections, haveRejections = expositionValue(last, "xbc_http_admission_rejections_total")
				if haveLimit && haveInFlight && haveRejections &&
					limit == wantLimit && inFlight == wantInFlight && rejections == wantRejections {
					return last
				}
			}
			select {
			case result := <-done:
				finished = true
				t.Fatalf("Execute returned while waiting for admission metrics: code=%d error=%v", result.code, result.err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf(
					"admission series = (limit %q, in_flight %q, rejections %q), want (%s, %s, %s); last scrape:\n%s",
					limit, inFlight, rejections, wantLimit, wantInFlight, wantRejections, last,
				)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// The holding request takes the only slot and blocks in its handler. Its
	// arrival in the handler is the fact that the process is at its ceiling.
	held := make(chan fetched, 1)
	heldErr := make(chan error, 1)
	go func() {
		response, requestErr := fetch("http://" + servingAddr + "/hold")
		if requestErr != nil {
			heldErr <- requestErr
			return
		}
		held <- response
	}()
	select {
	case <-entered:
	case requestErr := <-heldErr:
		t.Fatalf("the holding request failed: %v", requestErr)
	case <-time.After(admissionWait):
		t.Fatal("the holding request never reached its handler")
	}

	refused, refusedErr := fetch("http://" + servingAddr + "/hold")
	require.NoError(t, refusedErr)
	assert.Equal(t, http.StatusServiceUnavailable, refused.status,
		"a metered route is refused while the only admission slot is held")
	assert.Contains(t, refused.body, `"code":"server_overloaded"`,
		"the refusal names the admission condition, not a generic failure")

	// One snapshot, taken while the gate is saturated, must carry all three
	// readings: the ceiling that refused the request, the occupancy that
	// exhausted it, and the refusal itself.
	awaitAdmission(t, "1", "1", "1")

	releaseHolder()
	select {
	case response := <-held:
		assert.Equal(t, http.StatusOK, response.status, "the request that held the ceiling completes once released")
	case requestErr := <-heldErr:
		t.Fatalf("the holding request failed: %v", requestErr)
	case <-time.After(admissionWait):
		t.Fatal("the holding request did not finish")
	}

	// The release is asynchronous with respect to the client: poll until the
	// occupancy is back at zero. The rejection count is monotonic and must stay
	// visible.
	awaitAdmission(t, "1", "0", "1")

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

// TestAdmissionCollectorStaysDormantWithoutMetricsConfiguration pins the
// activation half of the collector's Definition. The collector follows
// plugins.metrics exactly like the request instrumentation: with that section
// absent the neutral metrics plugin exports no registry, so a collector
// Definition that was always enabled would fail planning with a missing
// producer instead of staying dormant with the rest of the instrumentation.
// Doctor is the read-only assembly path, which is what lets this test assert
// the planning outcome without binding a listener or constructing a plugin.
func TestAdmissionCollectorStaysDormantWithoutMetricsConfiguration(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "application.yml")
	config := `log:
  console:
    enabled: false
  file:
    enabled: false
`
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0o600))

	app, err := xbc.New(xbc.WithBundles(
		web.Bundle(), ginengine.Bundle(), metrics.Bundle()))
	require.NoError(t, err)

	code, err := app.Execute(context.Background(), []string{"doctor", "--config", configPath})
	require.NoError(t, err, "the admission collector must stay dormant when plugins.metrics is absent")
	assert.Equal(t, 0, code)
}
