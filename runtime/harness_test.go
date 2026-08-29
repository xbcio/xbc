package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/catalog"
)

// This file is the shared test harness for package xbc's own tests. Every
// test file in this package builds on it rather than re-deriving the same
// scaffolding, and adds only the fakes its own subject needs.
//
// Two conventions hold throughout:
//
//   - Tests never use the process-wide default catalog. They build and Freeze
//     a private catalog, then pass its Snapshot directly to newApp, so a
//     test's plugin set is exactly what it declared and is invisible to every
//     other test.
//   - Nothing is gated on a sleep. A timeout in this file is always an upper
//     bound on a failure, never the mechanism by which success is observed --
//     see waitFor and the ready channel.
type runResult struct {
	code int
	err  error
}

// testTimeout bounds how long a test will wait for something that should
// happen essentially immediately. It is generous because it is only ever
// reached when the test is already failing; nothing is timed against it.
const testTimeout = 10 * time.Second

// newTestApp builds an App over a private catalog containing exactly defs.
//
// The App's ready channel is created here, which is what lets a test wait for
// "startup finished, the run loop is now blocked" without polling.
func newTestApp(t *testing.T, defs ...plugin.Definition) *App {
	t.Helper()

	c := catalog.New()
	for _, d := range defs {
		c.Declare(d)
	}
	snapshot, err := c.Freeze()
	require.NoError(t, err, "冻结测试 catalog 失败")

	app := newApp(snapshot)
	app.ready = make(chan struct{})
	return app
}

// writeConfig writes body to a temp config file and returns the --config
// arguments that point at it.
func writeConfig(t *testing.T, body string) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600), "写入测试配置失败")
	return []string{"--config", path}
}

// quietConfig is the baseline configuration for tests that do not care about
// log output: console logging off, so a passing test prints nothing, and a
// short shutdown budget, so a test that deliberately hangs a Stop finishes in
// under a second instead of the 30s production default.
func quietConfig(t *testing.T, extra string) []string {
	t.Helper()
	return writeConfig(t, "log:\n  console:\n    enabled: false\n"+
		"xbc:\n  shutdown_timeout: 500ms\n"+extra)
}

// runAsync starts app.Execute on its own goroutine and hands back the channel its
// result will arrive on. Used by every test whose subject only becomes
// observable while the app is running -- signals, critical failures, shutdown
// ordering.
func runAsync(app *App, args ...string) <-chan runResult {
	ch := make(chan runResult, 1)
	go func() {
		code, err := app.Execute(context.Background(), args)
		ch <- runResult{code: code, err: err}
	}()
	return ch
}

// awaitReady blocks until the app has finished starting and entered its run
// loop, failing the test if it never does.
//
// Waiting on the ready channel rather than sleeping is what makes the tests
// below deterministic: ready is closed by wait, immediately before it blocks
// on stopCh, so a test that returns from here knows every startup stage has
// completed rather than merely hoping enough time has passed.
func awaitReady(t *testing.T, app *App) {
	t.Helper()
	select {
	case <-app.ready:
	case <-time.After(testTimeout):
		t.Fatal("应用未在超时内进入运行态")
	}
}

// awaitResult blocks until App.Execute returns, failing the test if it never
// does. A run that hangs is the single most likely symptom of a bug in the
// shutdown path, so it must fail rather than wedge the test binary.
func awaitResult(t *testing.T, ch <-chan runResult) runResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(testTimeout):
		t.Fatal("Execute 未在超时内返回，关机路径可能已挂死")
		return runResult{}
	}
}

// waitFor polls cond until it holds, failing with msg if it never does.
//
// It exists for the handful of conditions that are genuinely asynchronous and
// have no channel to wait on -- "the task runtime has stopped accepting
// submissions", for instance. The 200µs interval is a scheduling hint, not a
// correctness parameter: the loop is correct at any interval, and the timeout
// is an upper bound on failure rather than a gate on success.
func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Microsecond)
	}
	t.Fatalf("超时等待条件成立：%s", msg)
}

// --- fake plugins ---------------------------------------------------------
//
// The fakes are split by capability rather than combined into one
// do-everything type, because "which optional interfaces does this instance
// satisfy" is itself under test in several places. A single fake implementing
// every hook would make every instance report every capability, and the
// capability-detection assertions would pass no matter what the production
// code did.

// bare implements nothing beyond plugin.Plugin. It is the plugin with no
// lifecycle at all, used wherever a test needs an instance to exist without
// contributing behaviour.
type bare struct{ plugin.Base }

// hooked implements every core lifecycle interface, each delegating to a
// nil-able function field. Tests that care about ordering, failure
// propagation or shutdown behaviour use this and set only the hooks they are
// asserting on; an unset hook succeeds silently.
//
// Because it implements everything, hooked is the wrong choice for any test
// about capability detection -- use the narrow fakes below for that.
type hooked struct {
	plugin.Base

	onInit    func(ctx *plugin.Context) error
	onMigrate func(ctx *plugin.Context) error
	onStart   func(ctx *plugin.Context) error
	onOpen    func(ctx *plugin.Context) error
	onStop    func(ctx context.Context) error
}

var (
	_ plugin.Initializer   = (*hooked)(nil)
	_ plugin.Migrator      = (*hooked)(nil)
	_ plugin.Runner        = (*hooked)(nil)
	_ plugin.TrafficOpener = (*hooked)(nil)
	_ plugin.Closer        = (*hooked)(nil)
)

func (h *hooked) Init(ctx *plugin.Context) error {
	if h.onInit != nil {
		return h.onInit(ctx)
	}
	return nil
}

func (h *hooked) Migrate(ctx *plugin.Context) error {
	if h.onMigrate != nil {
		return h.onMigrate(ctx)
	}
	return nil
}

func (h *hooked) Start(ctx *plugin.Context) error {
	if h.onStart != nil {
		return h.onStart(ctx)
	}
	return nil
}

func (h *hooked) OpenTraffic(ctx *plugin.Context) error {
	if h.onOpen != nil {
		return h.onOpen(ctx)
	}
	return nil
}

func (h *hooked) Stop(ctx context.Context) error {
	if h.onStop != nil {
		return h.onStop(ctx)
	}
	return nil
}

// runnerOnly implements Runner and nothing else. Its main use is satisfying
// the liveness assertion in tests whose real subject is something else.
type runnerOnly struct {
	plugin.Base
	onStart func(ctx *plugin.Context) error
}

var _ plugin.Runner = (*runnerOnly)(nil)

func (r *runnerOnly) Start(ctx *plugin.Context) error {
	if r.onStart != nil {
		return r.onStart(ctx)
	}
	return nil
}

// initOnly implements Initializer and nothing else.
type initOnly struct {
	plugin.Base
	onInit func(ctx *plugin.Context) error
}

var _ plugin.Initializer = (*initOnly)(nil)

func (i *initOnly) Init(ctx *plugin.Context) error {
	if i.onInit != nil {
		return i.onInit(ctx)
	}
	return nil
}

// def is the boilerplate-free way to declare a fake: it builds a Definition
// whose Factory returns whatever make produces.
//
// Factory is called per instance, per App -- never shared -- so a test that
// needs to reach the constructed instance captures it from inside make rather
// than pre-building one and closing over it. That mirrors how real plugins
// work and is why there is no "declare this exact value" helper here.
func def(name string, make func() plugin.Plugin) plugin.Definition {
	return plugin.Definition{Key: plugin.Key(name), Factory: make}
}

// liveness returns a Definition for a plugin that does nothing but satisfy
// assertLiveness, so a test about some other subject does not fail startup
// for lack of anything long-running.
func liveness(name string) plugin.Definition {
	return def(name, func() plugin.Plugin { return new(runnerOnly) })
}

// --- log capture ----------------------------------------------------------

// logCapture reads back what the application actually logged.
//
// Some assertions cannot be made against in-process state without weakening
// them into proxies. "This shutdown produced no critical-failure log" is the
// clearest example: asserting on App.stopReason instead would test the
// variable that the log line is derived from, and would still pass if
// something else logged a spurious critical message. Reading the log file
// tests the observable the operator actually sees.
type logCapture struct {
	path string
}

// captureLogs points the logger at a JSON file in the test's temp dir and
// returns both the extra config that does so and a handle to read it back.
// Console output is disabled so a passing test stays silent.
func captureLogs(t *testing.T) (extra string, cap *logCapture) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.jsonl")
	return "log:\n  console:\n    enabled: false\n  file:\n    enabled: true\n    path: " + path + "\n",
		&logCapture{path: path}
}

// entry is one decoded log line. Only the fields the tests assert on are
// listed; zap writes more, and json.Unmarshal ignores the rest.
type entry struct {
	Level   string `json:"level"`
	Message string `json:"msg"`
	Plugin  string `json:"plugin"`
	Reason  string `json:"reason"`
	Error   string `json:"error"`
}

// entries flushes the logger and returns every line written so far.
func (c *logCapture) entries(t *testing.T) []entry {
	t.Helper()
	_ = log.Sync()

	raw, err := os.ReadFile(c.path)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err, "读取捕获的日志失败")

	var out []entry
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			// A line that is not JSON is not a test failure by itself: zap's
			// own internal writer errors surface this way. Skipping keeps the
			// capture useful instead of making every test brittle to them.
			continue
		}
		out = append(out, e)
	}
	return out
}

// containsMessage reports whether any captured line's message contains sub.
func (c *logCapture) containsMessage(t *testing.T, sub string) bool {
	t.Helper()
	for _, e := range c.entries(t) {
		if strings.Contains(e.Message, sub) {
			return true
		}
	}
	return false
}
