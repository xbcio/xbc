package apptest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/runtime"
	"github.com/xbcio/xbc/transport/web"
	ginengine "github.com/xbcio/xbc/transport/web/engines/gin"
)

const (
	// loopback keeps the test server off every externally reachable interface.
	loopback = "127.0.0.1"
	// baseConfigName is the generated file the runtime is started with.
	baseConfigName = "config.yaml"
	// overlayProfile is the --profile value New passes when WithYAML supplied
	// an overlay. The loader reads the sibling file named after the profile --
	// config-apptest.yaml for config.yaml -- so the overlay file name below has
	// to keep that shape; config.profileSibling owns the rule and is not
	// exported.
	overlayProfile = "apptest"
	// overlayConfigName is the sibling path the loader merges above the base
	// file.
	overlayConfigName = "config-" + overlayProfile + ".yaml"
	// shutdownTimeoutValue is written to xbc.shutdown_timeout. The framework's
	// 25s default would make every cleanup wait on a process that is already
	// idle, and a test process has no orchestrator granting it grace.
	shutdownTimeoutValue = "5s"
)

// App is an application started inside the test process and reachable over
// real HTTP on the loopback interface.
type App struct {
	// BaseURL is the origin the application listens on, without a trailing
	// slash, for example "http://127.0.0.1:53142".
	BaseURL string
	// Client is an HTTP client bounded by a request timeout, so a handler that
	// never answers fails the assertion instead of hanging the test.
	Client *http.Client
}

// URL joins a root-relative path to BaseURL.
func (a *App) URL(path string) string { return a.BaseURL + path }

// New starts the application under test and returns it once the probe route
// answers. The application is stopped when the test ends; a stop that reports
// an error fails the test from cleanup rather than being silently discarded.
func New(t testing.TB, opts ...Option) *App {
	t.Helper()
	app, err := launch(t, newOptions(opts...))
	if err != nil {
		t.Fatalf("apptest: %v", err)
	}
	return app
}

// launch starts one application, retrying a failed boot.
//
// The retry exists because the port is reserved by binding and closing a
// listener, so another process can claim it in the window before the
// application binds it; every attempt draws a fresh port. Composition is
// repeated per attempt as well, because a runtime.App executes exactly once.
func launch(t testing.TB, cfg options) (*App, error) {
	t.Helper()
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	// The temporary directory is taken once, before any attempt: the cleanup
	// it registers runs after the application is stopped, so the generated
	// files outlive every read the runtime makes of them.
	dir := t.TempDir()
	configPath := filepath.Join(dir, baseConfigName)
	args := []string{"--config", configPath}
	if cfg.yamlOverlay != "" {
		overlayPath := filepath.Join(dir, overlayConfigName)
		if err := os.WriteFile(overlayPath, []byte(cfg.yamlOverlay), 0o600); err != nil {
			return nil, fmt.Errorf("write configuration overlay: %w", err)
		}
		args = append(args, "--profile", overlayProfile)
	}

	bundles := make([]plugin.Bundle, 0, 3+len(cfg.bundles))
	bundles = append(bundles, Bundle(), web.Bundle(), ginengine.Bundle())
	bundles = append(bundles, cfg.bundles...)

	var lastErr error
	for attempt := 1; attempt <= maxStartAttempts; attempt++ {
		port, err := reservePort()
		if err != nil {
			return nil, fmt.Errorf("reserve a loopback port: %w", err)
		}
		if err := os.WriteFile(configPath, []byte(baseConfig(port)), 0o600); err != nil {
			return nil, fmt.Errorf("write configuration: %w", err)
		}

		app, err := boot(t, cfg, bundles, args, port)
		if err == nil {
			return app, nil
		}
		lastErr = err
		if attempt < maxStartAttempts {
			t.Logf("apptest: start attempt %d/%d failed: %v", attempt, maxStartAttempts, err)
		}
	}
	return nil, fmt.Errorf("application did not start after %d attempts: %w", maxStartAttempts, lastErr)
}

// boot runs one attempt: compose, execute, and wait for the probe. A failed
// attempt is fully stopped before it returns, so the next one starts from a
// clean process.
func boot(t testing.TB, cfg options, bundles []plugin.Bundle, args []string, port int) (*App, error) {
	t.Helper()
	application, err := runtime.New(runtime.WithBundles(bundles...))
	if err != nil {
		return nil, fmt.Errorf("compose runtime: %w", err)
	}

	baseURL := "http://" + net.JoinHostPort(loopback, strconv.Itoa(port))
	exec := startExecution(application, args)
	if err := awaitReady(exec, baseURL+readyPath, cfg.startTimeout); err != nil {
		result, stopped := exec.stop(shutdownBudget)
		if !stopped {
			return nil, fmt.Errorf("%w; the runtime also did not stop within %s after cancellation", err, shutdownBudget)
		}
		return nil, fmt.Errorf("%w; %s", err, describe(result))
	}

	t.Cleanup(func() {
		result, stopped := exec.stop(shutdownBudget)
		switch {
		case !stopped:
			t.Errorf("apptest: runtime did not stop within %s after its context was canceled", shutdownBudget)
		case result.err != nil && !errors.Is(result.err, context.Canceled):
			t.Errorf("apptest: runtime returned an error when stopped: %s", describe(result))
		}
	})
	return &App{BaseURL: baseURL, Client: &http.Client{Timeout: defaultClientTimeout}}, nil
}

// runResult is what one App.Execute call reported.
type runResult struct {
	code int
	err  error
}

func describe(result runResult) string {
	if result.err != nil {
		return fmt.Sprintf("exit code %d: %v", result.code, result.err)
	}
	return fmt.Sprintf("exit code %d", result.code)
}

// execution is one in-flight Execute call. The result channel is buffered so
// the goroutine driving Execute can always deliver and exit, even when nobody
// is waiting any more; result caches a value already consumed so a later stop
// does not wait for a second send that will never come.
type execution struct {
	cancel context.CancelFunc
	done   chan runResult
	result *runResult
}

func startExecution(application *runtime.App, args []string) *execution {
	ctx, cancel := context.WithCancel(context.Background())
	exec := &execution{cancel: cancel, done: make(chan runResult, 1)}
	go func() {
		code, err := application.Execute(ctx, args)
		exec.done <- runResult{code: code, err: err}
	}()
	return exec
}

// stop cancels Execute and waits for it to return. The budget only guards
// against a runtime that ignores cancellation; the runtime's own shutdown
// budget is configured separately in the generated file.
func (e *execution) stop(budget time.Duration) (runResult, bool) {
	e.cancel()
	if result, ok := e.poll(); ok {
		return result, true
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case result := <-e.done:
		e.result = &result
		return result, true
	case <-timer.C:
		return runResult{}, false
	}
}

// poll reports the result if Execute has already returned.
func (e *execution) poll() (runResult, bool) {
	if e.result != nil {
		return *e.result, true
	}
	select {
	case result := <-e.done:
		e.result = &result
		return result, true
	default:
		return runResult{}, false
	}
}

// awaitReady polls the probe route until it answers, Execute returns, or the
// budget expires. Execute returning first is reported immediately: waiting out
// the rest of the budget would only delay the diagnostic that explains why.
func awaitReady(exec *execution, url string, budget time.Duration) error {
	client := &http.Client{Timeout: probeTimeout}
	deadline := time.NewTimer(budget)
	defer deadline.Stop()
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()

	last := "no response"
	for {
		select {
		case result := <-exec.done:
			exec.result = &result
			return fmt.Errorf("runtime exited before %s answered", url)
		case <-ticker.C:
		case <-deadline.C:
			return fmt.Errorf("%s did not answer within %s (%s)", url, budget, last)
		}

		status, err := probe(client, url)
		switch {
		case err != nil:
			last = err.Error()
		case status == http.StatusOK:
			return nil
		default:
			last = "status " + strconv.Itoa(status)
		}
	}
}

func probe(client *http.Client, url string) (int, error) {
	response, err := client.Get(url)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return response.StatusCode, nil
}

// reservePort draws a free loopback port. The Web plugin does not export the
// address it binds, so the port has to be chosen before the application
// starts: this listener is opened and closed, and the application binds the
// same port moments later.
func reservePort() (int, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(loopback, "0"))
	if err != nil {
		return 0, err
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return 0, fmt.Errorf("unexpected listener address %s", listener.Addr())
	}
	if err := listener.Close(); err != nil {
		return 0, err
	}
	return address.Port, nil
}

func baseConfig(port int) string {
	return fmt.Sprintf(
		"web:\n  addr: %s\nxbc:\n  shutdown_timeout: %s\n",
		net.JoinHostPort(loopback, strconv.Itoa(port)),
		shutdownTimeoutValue,
	)
}
