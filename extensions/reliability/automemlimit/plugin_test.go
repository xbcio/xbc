package automemlimit

import (
	"context"
	"errors"
	"math"
	"os"
	"reflect"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KimMachineGun/automemlimit/memlimit"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

// Package-level handles are the seam between the behavior tests and the two
// fakes below: the provider the tests install, and the managed task the fake
// host records. Tests in this package run sequentially because they change the
// process-global soft memory limit; resetTestHandles restores every handle in
// t.Cleanup so a failure cannot leak into the next test.
var (
	testProviderMu    sync.Mutex
	testProviderLimit uint64
	testProviderErr   error
	testProviderCalls atomic.Int64

	testSubmitRefused       bool
	testSubmittedTasks      []func(context.Context)
	testSubmittedIdentities []plugin.Identity
)

// resetTestHandles installs the package-level defaults and restores them after
// the test.
func resetTestHandles(t *testing.T) {
	t.Helper()
	reset := func() {
		setTestProvider(0, nil)
		testProviderCalls.Store(0)
		testSubmitRefused = false
		testSubmittedTasks = nil
		testSubmittedIdentities = nil
	}
	reset()
	t.Cleanup(reset)
}

// setTestProvider is the race-free way to change what the fake provider
// returns while the refresh task may be reading it.
func setTestProvider(limit uint64, err error) {
	testProviderMu.Lock()
	defer testProviderMu.Unlock()
	testProviderLimit = limit
	testProviderErr = err
}

// testProvider is the injected memlimit.Provider. It counts its calls so a
// test can prove the plugin consulted it (or did not).
func testProvider() (uint64, error) {
	testProviderCalls.Add(1)
	testProviderMu.Lock()
	defer testProviderMu.Unlock()
	return testProviderLimit, testProviderErr
}

// saveProcessLimit records the soft limit in force and restores it after the
// test. Every test that lets the plugin or a fake install a limit calls it.
func saveProcessLimit(t *testing.T) {
	t.Helper()
	previous := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(previous) })
}

// clearMemlimitEnvironment removes the two variables the library reads, so a
// developer's own shell cannot decide what a test observes, and restores them
// afterwards.
func clearMemlimitEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{envGOMEMLIMIT, envAUTOMEMLIMIT} {
		value, present := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(name, value)
				return
			}
			_ = os.Unsetenv(name)
		})
	}
}

// recordedLine is one log entry the recording logger kept.
type recordedLine struct {
	level string
	msg   string
	kv    []any
}

// recordingLogger is a log.Logger that keeps what it was told, so the tests
// can assert the warning a degraded startup emits.
type recordingLogger struct {
	mu    sync.Mutex
	lines []recordedLine
}

func (l *recordingLogger) record(level, msg string, kv []any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, recordedLine{level: level, msg: msg, kv: append([]any(nil), kv...)})
}

func (l *recordingLogger) Debug(msg string, kv ...any) { l.record("DEBUG", msg, kv) }
func (l *recordingLogger) Info(msg string, kv ...any)  { l.record("INFO", msg, kv) }
func (l *recordingLogger) Warn(msg string, kv ...any)  { l.record("WARN", msg, kv) }
func (l *recordingLogger) Error(msg string, kv ...any) { l.record("ERROR", msg, kv) }
func (l *recordingLogger) Fatal(msg string, kv ...any) { l.record("FATAL", msg, kv) }
func (l *recordingLogger) With(_ ...any) log.Logger    { return l }
func (l *recordingLogger) Enabled(log.Level) bool      { return true }

var _ log.Logger = (*recordingLogger)(nil)

func (l *recordingLogger) count(level string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for _, line := range l.lines {
		if line.level == level {
			count++
		}
	}
	return count
}

func (l *recordingLogger) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var builder strings.Builder
	for _, line := range l.lines {
		builder.WriteString(line.level)
		builder.WriteString(" ")
		builder.WriteString(line.msg)
		builder.WriteString("\n")
	}
	return builder.String()
}

// testHost is the RuntimeHost half of the plugin.Context the tests build
// directly.
type testHost struct {
	mu     sync.Mutex
	logger log.Logger
}

var _ plugin.RuntimeHost = (*testHost)(nil)

func newTestHost(logger log.Logger) *testHost {
	return &testHost{logger: logger}
}

func (*testHost) ExecutionContext() context.Context { return context.Background() }
func (h *testHost) Logger() log.Logger              { return h.logger }
func (*testHost) ProcessInstance() string           { return "test-process" }

func (*testHost) Service() plugin.ServiceIdentity { return plugin.ServiceIdentity{} }

func (*testHost) IngressStopBudget() time.Duration { return 0 }
func (*testHost) TrafficGate() <-chan struct{}     { return nil }

func (*testHost) Admission(plugin.Identity) plugin.Admission { return nil }

func (*testHost) AdmissionFor(plugin.Identity, plugin.WorkloadKey) plugin.Admission { return nil }

func (*testHost) RequestShutdown(plugin.Identity, string) bool { return false }

// SubmitTask records the managed task instead of running it, so the refresh
// test drives the exact function Start submitted.
func (h *testHost) SubmitTask(id plugin.Identity, fn func(context.Context), _ bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if testSubmitRefused {
		return false
	}
	testSubmittedTasks = append(testSubmittedTasks, fn)
	testSubmittedIdentities = append(testSubmittedIdentities, id)
	return true
}

func (h *testHost) submitted() ([]func(context.Context), []plugin.Identity) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]func(context.Context){}, testSubmittedTasks...),
		append([]plugin.Identity{}, testSubmittedIdentities...)
}

// newPluginFromConfig builds the plugin the way the Definition's factory does,
// then injects the fake provider.
func newPluginFromConfig(t *testing.T, mutate func(*Config)) *Plugin {
	t.Helper()
	cfg := DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatalf("normalizeConfig(%#v) = %v", cfg, err)
	}
	p := newPlugin(normalized)
	p.provider = testProvider
	return p
}

func newRuntimeContext(logger log.Logger) (*plugin.Context, *testHost) {
	host := newTestHost(logger)
	return plugin.NewRuntimeContext(host, plugin.Identity{Plugin: Key}), host
}

// TestDefinitionIsCanonicalAndAlwaysEnabled pins the declaration shape: one
// stable handle, one Bundle entry, a ConfigSpec, and the zero Activation that
// makes selecting the Bundle the explicit decision.
func TestDefinitionIsCanonicalAndAlwaysEnabled(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero || Definition() != Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}
	bundle := Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(bundle, Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}

	entries := pluginmodel.BundleEntries(pluginmodel.Bundle(bundle))
	if len(entries) != 1 {
		t.Fatalf("Bundle() entries = %d, want 1", len(entries))
	}
	if !pluginmodel.SameDefinition(entries[0].Definition, pluginmodel.Definition(Definition())) {
		t.Fatal("Bundle() does not contain the canonical Definition handle")
	}

	descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(Definition()))
	if !ok {
		t.Fatal("Definition() returned a zero handle")
	}
	if descriptor.Key != pluginmodel.Key(Key) {
		t.Fatalf("Definition key = %v, want %v", descriptor.Key, Key)
	}
	if descriptor.Activation.Kind != pluginmodel.ActivationAlways {
		t.Fatalf("Definition activation = %+v, want the zero always-enabled policy", descriptor.Activation)
	}
	if descriptor.Primary != reflect.TypeOf((*Plugin)(nil)) {
		t.Fatalf("Definition primary = %v, want *Plugin", descriptor.Primary)
	}
	if descriptor.Config == nil {
		t.Fatal("Definition declares no ConfigSpec")
	}
	if len(descriptor.Contracts) != 0 {
		t.Fatalf("Definition contracts = %+v, want no exports beyond the primary", descriptor.Contracts)
	}
}

// TestInitInstallsTheConfiguredRatioOfTheContainerLimit is the core behavior:
// the provider's limit passes through the configured ratio, not the library's
// own default, and the result becomes the process soft limit.
func TestInitInstallsTheConfiguredRatioOfTheContainerLimit(t *testing.T) {
	resetTestHandles(t)
	saveProcessLimit(t)
	clearMemlimitEnvironment(t)

	const cgroupLimit = uint64(1 << 30)
	setTestProvider(cgroupLimit, nil)
	p := newPluginFromConfig(t, func(cfg *Config) { cfg.Ratio = 0.5 })

	ctx, _ := newRuntimeContext(log.Nop())
	if err := p.init(ctx); err != nil {
		t.Fatalf("Init() = %v", err)
	}

	want := int64(float64(cgroupLimit) * 0.5)
	if got := debug.SetMemoryLimit(-1); got != want {
		t.Fatalf("soft limit = %d, want %d", got, want)
	}
	if calls := testProviderCalls.Load(); calls != 1 {
		t.Fatalf("provider calls = %d, want 1", calls)
	}
}

// TestInitRaisesTheResultToMinBytes covers the minimum: 90% of 100 MiB is
// below the 200 MiB floor.
func TestInitRaisesTheResultToMinBytes(t *testing.T) {
	resetTestHandles(t)
	saveProcessLimit(t)
	clearMemlimitEnvironment(t)

	const (
		cgroupLimit = uint64(100 << 20)
		minBytes    = int64(200 << 20)
	)
	setTestProvider(cgroupLimit, nil)
	p := newPluginFromConfig(t, func(cfg *Config) { cfg.MinBytes = minBytes })

	ctx, _ := newRuntimeContext(log.Nop())
	if err := p.init(ctx); err != nil {
		t.Fatalf("Init() = %v", err)
	}
	if got := debug.SetMemoryLimit(-1); got != minBytes {
		t.Fatalf("soft limit = %d, want the min_bytes floor %d", got, minBytes)
	}
}

// TestInitSkipsWhenGOMEMLIMITIsInTheEnvironment also proves the ordering: a
// non-default limit is installed first, so an implementation that checked the
// conflict before the environment would fail this test instead of skipping.
func TestInitSkipsWhenGOMEMLIMITIsInTheEnvironment(t *testing.T) {
	resetTestHandles(t)
	saveProcessLimit(t)
	clearMemlimitEnvironment(t)

	const installed = int64(128 << 20)
	debug.SetMemoryLimit(installed)
	t.Setenv(envGOMEMLIMIT, "64MiB")
	setTestProvider(1<<30, nil)

	p := newPluginFromConfig(t, nil)
	ctx, _ := newRuntimeContext(log.Nop())
	if err := p.init(ctx); err != nil {
		t.Fatalf("Init() = %v; an environment-provided GOMEMLIMIT must skip the conflict check", err)
	}
	if got := debug.SetMemoryLimit(-1); got != installed {
		t.Fatalf("soft limit = %d, want the pre-existing %d", got, installed)
	}
	if calls := testProviderCalls.Load(); calls != 0 {
		t.Fatalf("provider calls = %d, want 0", calls)
	}
}

// TestInitSkipsWhenAUTOMEMLIMITIsOff pins the second environment escape hatch,
// with the same non-default-limit setup that would otherwise be a conflict.
func TestInitSkipsWhenAUTOMEMLIMITIsOff(t *testing.T) {
	resetTestHandles(t)
	saveProcessLimit(t)
	clearMemlimitEnvironment(t)

	const installed = int64(128 << 20)
	debug.SetMemoryLimit(installed)
	t.Setenv(envAUTOMEMLIMIT, "off")
	setTestProvider(1<<30, nil)

	p := newPluginFromConfig(t, nil)
	ctx, _ := newRuntimeContext(log.Nop())
	if err := p.init(ctx); err != nil {
		t.Fatalf("Init() = %v; AUTOMEMLIMIT=off must skip the conflict check", err)
	}
	if got := debug.SetMemoryLimit(-1); got != installed {
		t.Fatalf("soft limit = %d, want the pre-existing %d", got, installed)
	}
	if calls := testProviderCalls.Load(); calls != 0 {
		t.Fatalf("provider calls = %d, want 0", calls)
	}
}

// TestInitRefusesToCompeteWithAnInstalledLimit pins the mutual exclusion with
// xbc.runtime.memory_limit: the error names both owners and the limit in
// force, and the installed value is untouched.
func TestInitRefusesToCompeteWithAnInstalledLimit(t *testing.T) {
	resetTestHandles(t)
	saveProcessLimit(t)
	clearMemlimitEnvironment(t)

	const installed = int64(64 << 20)
	debug.SetMemoryLimit(installed)
	setTestProvider(1<<30, nil)

	p := newPluginFromConfig(t, nil)
	ctx, _ := newRuntimeContext(log.Nop())
	err := p.init(ctx)
	if err == nil {
		t.Fatal("Init() succeeded while another owner had installed a soft memory limit")
	}
	for _, want := range []string{
		strconv.FormatInt(installed, 10),
		"xbc.runtime.memory_limit",
		"plugins.automemlimit",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Init() error = %q, want it to contain %q", err, want)
		}
	}
	if got := debug.SetMemoryLimit(-1); got != installed {
		t.Fatalf("soft limit = %d, want the pre-existing %d", got, installed)
	}
	if calls := testProviderCalls.Load(); calls != 0 {
		t.Fatalf("provider calls = %d, want 0: the conflict is decided before the provider runs", calls)
	}
}

// TestInitWarnsAndContinuesWithoutACgroupLimit covers both ordinary "there is
// no container limit here" errors: startup succeeds, the limit is untouched,
// and exactly one warning explains the degradation.
func TestInitWarnsAndContinuesWithoutACgroupLimit(t *testing.T) {
	for name, providerErr := range map[string]error{
		"cgroups unsupported": memlimit.ErrCgroupsNotSupported,
		"no cgroup":           memlimit.ErrNoCgroup,
	} {
		t.Run(name, func(t *testing.T) {
			resetTestHandles(t)
			saveProcessLimit(t)
			clearMemlimitEnvironment(t)
			setTestProvider(0, providerErr)

			logger := &recordingLogger{}
			p := newPluginFromConfig(t, nil)
			ctx, _ := newRuntimeContext(logger)
			if err := p.init(ctx); err != nil {
				t.Fatalf("Init() = %v, want a non-fatal degradation", err)
			}
			if got := debug.SetMemoryLimit(-1); got != math.MaxInt64 {
				t.Fatalf("soft limit = %d, want the unchanged %d", got, int64(math.MaxInt64))
			}
			if warns := logger.count("WARN"); warns != 1 {
				t.Fatalf("warnings = %d, want exactly 1: %s", warns, logger.text())
			}
		})
	}
}

// TestInitTreatsNoFiniteLimitAsSuccess covers a provider that reports an
// unlimited container: the library installs math.MaxInt64, which is the value
// already in force.
func TestInitTreatsNoFiniteLimitAsSuccess(t *testing.T) {
	resetTestHandles(t)
	saveProcessLimit(t)
	clearMemlimitEnvironment(t)
	setTestProvider(0, memlimit.ErrNoLimit)

	logger := &recordingLogger{}
	p := newPluginFromConfig(t, nil)
	ctx, _ := newRuntimeContext(logger)
	if err := p.init(ctx); err != nil {
		t.Fatalf("Init() = %v", err)
	}
	if got := debug.SetMemoryLimit(-1); got != math.MaxInt64 {
		t.Fatalf("soft limit = %d, want the unchanged %d", got, int64(math.MaxInt64))
	}
	if !strings.Contains(logger.text(), "no finite memory limit") {
		t.Fatalf("log = %q, want the no-finite-limit line", logger.text())
	}
}

// TestInitFailsOnARealProviderError distinguishes an unreadable cgroup from an
// absent one: the process must not start with a policy it could not apply.
func TestInitFailsOnARealProviderError(t *testing.T) {
	resetTestHandles(t)
	saveProcessLimit(t)
	clearMemlimitEnvironment(t)
	setTestProvider(0, errors.New("permission denied"))

	p := newPluginFromConfig(t, nil)
	ctx, _ := newRuntimeContext(log.Nop())
	err := p.init(ctx)
	if err == nil {
		t.Fatal("Init() succeeded on a provider failure")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("Init() error = %q, want it to carry the provider error", err)
	}
	if got := debug.SetMemoryLimit(-1); got != math.MaxInt64 {
		t.Fatalf("soft limit = %d, want the unchanged %d", got, int64(math.MaxInt64))
	}
}

// TestInitFailsOnAnInvalidAUTOMEMLIMIT covers the library's own parse error:
// the environment override is rejected rather than silently ignored.
func TestInitFailsOnAnInvalidAUTOMEMLIMIT(t *testing.T) {
	resetTestHandles(t)
	saveProcessLimit(t)
	clearMemlimitEnvironment(t)
	t.Setenv(envAUTOMEMLIMIT, "half")
	setTestProvider(1<<30, nil)

	p := newPluginFromConfig(t, nil)
	ctx, _ := newRuntimeContext(log.Nop())
	err := p.init(ctx)
	if err == nil {
		t.Fatal("Init() succeeded with an unparsable AUTOMEMLIMIT")
	}
	if !strings.Contains(err.Error(), envAUTOMEMLIMIT) {
		t.Fatalf("Init() error = %q, want it to name AUTOMEMLIMIT", err)
	}
	if got := debug.SetMemoryLimit(-1); got != math.MaxInt64 {
		t.Fatalf("soft limit = %d, want the unchanged %d", got, int64(math.MaxInt64))
	}
}

// TestStartWithoutARefreshIntervalSubmitsNoTask pins the zero-interval
// contract: Start returns without asking the runtime for anything.
func TestStartWithoutARefreshIntervalSubmitsNoTask(t *testing.T) {
	resetTestHandles(t)
	saveProcessLimit(t)
	clearMemlimitEnvironment(t)
	setTestProvider(1<<30, nil)

	p := newPluginFromConfig(t, nil)
	ctx, host := newRuntimeContext(log.Nop())
	if err := p.init(ctx); err != nil {
		t.Fatalf("Init() = %v", err)
	}
	if err := p.start(ctx); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	if tasks, _ := host.submitted(); len(tasks) != 0 {
		t.Fatalf("submitted tasks = %d, want none for refresh_interval 0", len(tasks))
	}
}

// TestStartRefreshesThroughOneManagedTask drives the submitted task directly:
// the first tick re-reads the changed provider limit and re-applies it, and
// cancelling the task context ends the loop.
func TestStartRefreshesThroughOneManagedTask(t *testing.T) {
	resetTestHandles(t)
	saveProcessLimit(t)
	clearMemlimitEnvironment(t)
	setTestProvider(1<<30, nil)

	p := newPluginFromConfig(t, func(cfg *Config) {
		cfg.RefreshInterval = 5 * time.Millisecond
	})
	ctx, host := newRuntimeContext(log.Nop())
	if err := p.init(ctx); err != nil {
		t.Fatalf("Init() = %v", err)
	}
	if err := p.start(ctx); err != nil {
		t.Fatalf("Start() = %v", err)
	}

	tasks, identities := host.submitted()
	if len(tasks) != 1 {
		t.Fatalf("submitted tasks = %d, want exactly 1 managed refresh task", len(tasks))
	}
	if identities[0].Plugin != Key {
		t.Fatalf("submitted identity = %v, want plugin %v", identities[0], Key)
	}

	const refreshedLimit = uint64(2 << 30)
	setTestProvider(refreshedLimit, nil)
	ratio := p.config.ratio
	want := int64(float64(refreshedLimit) * ratio)

	taskCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tasks[0](taskCtx)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for debug.SetMemoryLimit(-1) != want {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("refresh did not update the soft limit to %d", want)
		}
		time.Sleep(time.Millisecond)
	}
	if calls := testProviderCalls.Load(); calls < 2 {
		cancel()
		<-done
		t.Fatalf("provider calls = %d, want at least 2 (the refresh must re-read)", calls)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh task did not exit after its context was cancelled")
	}
}

// TestStartFailsWhenTheTaskSubmissionIsRefused keeps a refused submission from
// silently dropping the configured refresh.
func TestStartFailsWhenTheTaskSubmissionIsRefused(t *testing.T) {
	resetTestHandles(t)
	saveProcessLimit(t)
	clearMemlimitEnvironment(t)
	setTestProvider(1<<30, nil)
	testSubmitRefused = true

	p := newPluginFromConfig(t, func(cfg *Config) {
		cfg.RefreshInterval = time.Second
	})
	ctx, _ := newRuntimeContext(log.Nop())
	if err := p.init(ctx); err != nil {
		t.Fatalf("Init() = %v", err)
	}
	err := p.start(ctx)
	if err == nil {
		t.Fatal("Start() succeeded although the runtime refused the managed task")
	}
	if !strings.Contains(err.Error(), "managed task submission was refused") {
		t.Fatalf("Start() error = %q, want the refusal named", err)
	}
}

// TestProviderFromSelectsTheFallbackMode is the positive control for the
// fallback option: "none" exposes the primary error, "system" falls back to
// the secondary source, and a successful primary wins in both modes.
func TestProviderFromSelectsTheFallbackMode(t *testing.T) {
	failing := func() (uint64, error) { return 0, memlimit.ErrCgroupsNotSupported }
	systemLimit := func() (uint64, error) { return 4 << 30, nil }
	succeeding := func() (uint64, error) { return 1 << 30, nil }

	none := providerFrom(normalizedConfig{fallback: fallbackNone}, failing, systemLimit)
	if _, err := none(); !errors.Is(err, memlimit.ErrCgroupsNotSupported) {
		t.Fatalf("fallback none provider error = %v, want the primary error", err)
	}

	system := providerFrom(normalizedConfig{fallback: fallbackSystem}, failing, systemLimit)
	limit, err := system()
	if err != nil || limit != 4<<30 {
		t.Fatalf("fallback system provider = (%d, %v), want (4GiB, nil)", limit, err)
	}

	system = providerFrom(normalizedConfig{fallback: fallbackSystem}, succeeding, systemLimit)
	limit, err = system()
	if err != nil || limit != 1<<30 {
		t.Fatalf("fallback system with a working primary = (%d, %v), want (1GiB, nil)", limit, err)
	}
}
