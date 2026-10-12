package runtime

import (
	"context"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// withBuildInfo hands the fallback chain a documented build for the duration
// of one test, so the assertions do not depend on whatever this repository's
// own build happens to carry. Tests using it must not call t.Parallel:
// readBuildInfo is the package's seam over process-wide build metadata, and
// every parallel test's loadSettings reads it.
func withBuildInfo(t *testing.T, info *debug.BuildInfo) {
	t.Helper()
	previous := readBuildInfo
	readBuildInfo = func() *debug.BuildInfo { return info }
	t.Cleanup(func() { readBuildInfo = previous })
}

// TestServiceNameFallsBackFromTheModulePathToTheExecutableName pins the name
// half of the fallback chain: the last segment of the main module path --
// what a build records -- and the executable's file name when the build
// records no module path at all.
func TestServiceNameFallsBackFromTheModulePathToTheExecutableName(t *testing.T) {
	withBuildInfo(t, &debug.BuildInfo{Main: debug.Module{Path: "github.com/acme/orders-api"}})
	loaded, err := loadSettings(settingsEnvironment(t, nil))
	require.NoError(t, err)
	assert.Equal(t, "orders-api", loaded.ServiceIdentity().Name,
		"the module path's last segment is the service name")

	withBuildInfo(t, nil)
	loaded, err = loadSettings(settingsEnvironment(t, nil))
	require.NoError(t, err)
	assert.Equal(t, filepath.Base(os.Args[0]), loaded.ServiceIdentity().Name,
		"a build with no recorded module path falls back to the executable name")
}

// TestServiceVersionFallsBackInBuildRecordedOrder pins the version half:
// the main module's version, unless the toolchain's "(devel)" placeholder (or
// the empty string); the VCS revision shortened to 12 characters, marked
// "-dirty" when the tree had local modifications; and "unknown" when the
// build records neither.
func TestServiceVersionFallsBackInBuildRecordedOrder(t *testing.T) {
	for _, tc := range []struct {
		name string
		info *debug.BuildInfo
		want string
	}{
		{
			name: "the main module version wins over the revision",
			info: &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"},
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"}}},
			want: "v1.2.3",
		},
		{
			name: "(devel) is excluded and the revision is shortened",
			info: &debug.BuildInfo{Main: debug.Module{Version: "(devel)"},
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"}}},
			want: "0123456789ab",
		},
		{
			name: "a dirty tree marks the revision",
			info: &debug.BuildInfo{Main: debug.Module{Version: "(devel)"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"},
					{Key: "vcs.modified", Value: "true"},
				}},
			want: "0123456789ab-dirty",
		},
		{
			name: "an empty main version falls through to the revision",
			info: &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "aabbccddeeff00112233445566778899aabbccdd"}}},
			want: "aabbccddeeff",
		},
		{
			name: "no recorded version and no revision is unknown",
			info: &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			want: "unknown",
		},
		{
			name: "a stripped build is unknown",
			info: nil,
			want: "unknown",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withBuildInfo(t, tc.info)
			loaded, err := loadSettings(settingsEnvironment(t, nil))
			require.NoError(t, err)
			assert.Equal(t, tc.want, loaded.ServiceIdentity().Version)
		})
	}
}

// TestServiceIdentityConfigurationBeatsTheBuildFallback pins that the two
// configured keys are the first links of the chain, not overrides applied
// afterwards: a deployment that names its service and version gets exactly
// those, however the build describes itself. It also pins that the identity
// carries the same instance every other consumer reports.
func TestServiceIdentityConfigurationBeatsTheBuildFallback(t *testing.T) {
	withBuildInfo(t, &debug.BuildInfo{Main: debug.Module{Path: "github.com/acme/orders-api", Version: "v1.2.3"}})
	loaded, err := loadSettings(settingsEnvironment(t, map[string]any{"xbc": map[string]any{
		"service": map[string]any{"name": "checkout", "version": "canary-7"},
	}}))
	require.NoError(t, err)

	identity := loaded.ServiceIdentity()
	assert.Equal(t, "checkout", identity.Name)
	assert.Equal(t, "canary-7", identity.Version)
	assert.Equal(t, loaded.Instance(), identity.Instance,
		"the identity's instance is the process-wide instance, not a second source")
}

// TestServiceIdentityRejectsWhitespace pins the validation rule the spec
// imports from instance_id: both values are presented verbatim as this
// service's identity, so an embedded space is a mistake, not a formatting
// choice.
func TestServiceIdentityRejectsWhitespace(t *testing.T) {
	for _, tc := range []struct {
		field   string
		value   string
		message string
	}{
		{field: "name", value: "orders api", message: "service.name must not contain whitespace"},
		{field: "version", value: "v1 2", message: "service.version must not contain whitespace"},
	} {
		_, err := loadSettings(settingsEnvironment(t, map[string]any{"xbc": map[string]any{
			"service": map[string]any{tc.field: tc.value},
		}}))
		require.Error(t, err, "%s = %q must fail startup", tc.field, tc.value)
		assert.ErrorContains(t, err, tc.message)
		assert.ErrorContains(t, err, tc.value, "the error shows the offending value")
	}
}

// TestServiceIdentityIsNeverEmptyWithoutConfiguration pins the whole point of
// the fallback chain: a deployment that never writes xbc.service still gets a
// truthful identity instead of an empty one, because every JSON log line and
// exported span carries it.
func TestServiceIdentityIsNeverEmptyWithoutConfiguration(t *testing.T) {
	t.Parallel()
	loaded, err := loadSettings(settingsEnvironment(t, nil))
	require.NoError(t, err)

	identity := loaded.ServiceIdentity()
	assert.NotEmpty(t, identity.Name)
	assert.NotEmpty(t, identity.Version)
	assert.NotEmpty(t, identity.Instance)
	assert.Equal(t, loaded.Instance(), identity.Instance)
}

// TestIngressStopBudgetIsZeroBeforeBinding pins the documented free meaning of
// the zero value: settings that were never bound carry no budgets, which is
// what lets a transport read 0 as "cannot judge" rather than "no time".
func TestIngressStopBudgetIsZeroBeforeBinding(t *testing.T) {
	t.Parallel()
	assert.Zero(t, settings{}.IngressStopBudget())
}

// TestHostAdapterReportsOneInstanceForBothSpellings pins the runtime's own
// reverse port: ProcessInstance and Service().Instance are two readings of
// the one bound identity, so a plugin that reaches for either names the same
// process -- and the identity carries the configured name and version, not a
// fallback.
func TestHostAdapterReportsOneInstanceForBothSpellings(t *testing.T) {
	app := newRuntimeTestApp()
	path := writeRuntimeTestConfig(t, "xbc:\n  service:\n    name: orders\n    version: v1\n")
	cmd, err := parseArgs([]string{"doctor", "--config", path}, config.DefaultEnvPrefix, nil)
	require.NoError(t, err)
	require.NoError(t, app.bootstrap(cmd))

	host := hostAdapter{app: app}
	assert.Equal(t, host.ProcessInstance(), host.Service().Instance)
	assert.Equal(t, "orders", host.Service().Name)
	assert.Equal(t, "v1", host.Service().Version)
}

// TestBootedJSONSinkCarriesTheConfiguredIdentity pins the bootstrap ordering
// the identity consumer depends on: loadSettings settles the service identity
// before log.Init assembles the sinks, so the very first JSON record a real
// boot writes already carries xbc.service's name and version. Were the logger
// assembled first, its encoders would hold a zero identity and every record
// would come out unidentified.
func TestBootedJSONSinkCarriesTheConfiguredIdentity(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.jsonl")
	path := writeRuntimeTestConfig(t,
		"log:\n  console:\n    enabled: false\n  file:\n    enabled: true\n    path: "+logPath+"\n"+
			"xbc:\n  service:\n    name: orders\n    version: v9\n")
	app := newRuntimeTestApp()
	cmd, err := parseArgs([]string{"--config", path}, config.DefaultEnvPrefix, nil)
	require.NoError(t, err)
	require.NoError(t, app.bootstrap(cmd))
	t.Cleanup(func() { _ = log.Close(); log.SetLogger(log.Nop()) })

	log.L().Info("boot line")
	require.NoError(t, log.Close())

	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"service.name":"orders"`)
	assert.Contains(t, string(data), `"service.version":"v9"`)
	assert.Contains(t, string(data), `"service.instance.id":"`+app.settings.Instance()+`"`)
}

// wiringObservation records what the identity and budget seams answered on a
// real boot. Both hooks run on the runtime's own goroutines, so the reads are
// guarded and taken after the application has finished.
type wiringObservation struct {
	mu             sync.Mutex
	buildService   plugin.ServiceIdentity
	contextService plugin.ServiceIdentity
	budget         time.Duration
}

// TestConstructionWiringCarriesTheIdentityAndBudgetToPlugins pins the whole
// seam S1 and S2 promised, end to end on a real boot: the identity settled in
// settings reaches a Definition factory through BuildContext.Service, a
// running plugin through Context.Service, and the ingress budget through
// Context.IngressStopBudget -- each equal to the bound settings' own reading,
// which the shutdown walk and the startup report draw from. Every S1 consumer
// (tracing's resource, build_info, the JSON sinks) and S2's transport check
// receive their values through exactly this wiring, so a break anywhere on it
// would silently hollow all of them out while each consumer's own unit test,
// which injects its identity directly, stayed green.
func TestConstructionWiringCarriesTheIdentityAndBudgetToPlugins(t *testing.T) {
	const (
		shutdownTimeout = 6 * time.Second
		stopReserve     = 2 * time.Second
	)
	observation := &wiringObservation{}
	definition := plugin.Define("wiring-probe", func(ctx plugin.BuildContext) (*runtimeTestValue, error) {
		observation.mu.Lock()
		observation.buildService = ctx.Service()
		observation.mu.Unlock()
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		OpenTraffic: func(_ *runtimeTestValue, ctx *plugin.Context) error {
			observation.mu.Lock()
			observation.contextService = ctx.Service()
			observation.budget = ctx.IngressStopBudget()
			observation.mu.Unlock()
			return nil
		},
		Stop: func(*runtimeTestValue, context.Context) error { return nil },
	}})

	app := newRuntimeTestApp(definition)
	path := writeRuntimeTestConfig(t,
		"log:\n  console:\n    enabled: false\n  file:\n    enabled: false\n"+
			"xbc:\n  shutdown_timeout: "+shutdownTimeout.String()+"\n  drain_timeout: 1s\n  pre_stop_timeout: 0s\n"+
			"  stop_reserve: "+stopReserve.String()+"\n  instance_id: host-7-abc\n"+
			"  service:\n    name: orders\n    version: v9\n")

	result := executeRuntimeTest(app, "--config", path)
	awaitRuntimeTestReady(t, app)
	require.True(t, app.requestStop(stopReasonSignal))
	completed := awaitRuntimeTestResult(t, result)
	require.NoError(t, completed.err)
	require.Equal(t, 0, completed.code)

	observation.mu.Lock()
	buildService := observation.buildService
	contextService := observation.contextService
	budget := observation.budget
	observation.mu.Unlock()

	wantIdentity := app.settings.ServiceIdentity()
	assert.Equal(t, plugin.ServiceIdentity{Name: "orders", Version: "v9", Instance: "host-7-abc"}, wantIdentity,
		"the bound settings settle the configured identity")
	assert.Equal(t, wantIdentity, buildService,
		"a Definition factory reads the bound identity through BuildContext.Service")
	assert.Equal(t, wantIdentity, contextService,
		"a running plugin reads the same identity through Context.Service")
	assert.Equal(t, shutdownTimeout-stopReserve, app.settings.IngressStopBudget(),
		"the bound settings settle the budget as shutdown_timeout less stop_reserve")
	assert.Equal(t, app.settings.IngressStopBudget(), budget,
		"Context.IngressStopBudget reports the same budget the shutdown walk derives phase A's deadline from")
}
