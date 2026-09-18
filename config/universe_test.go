package config

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// frameworkSection is the shape of the framework's own "xbc" root: a typed
// section whose path begins by spelling the process prefix. It is declared
// separately from serverSection because that collision is the whole subject of
// the tests below.
type frameworkSection struct {
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" default:"30s"`
	InstanceID      string        `yaml:"instance_id"`
}

// xbcRootUniverse declares the framework's root beside an ordinary one, so a
// test can show that only the root spelling the process prefix collapses.
func xbcRootUniverse(t *testing.T) *Universe {
	t.Helper()
	universe, err := NewUniverse(
		Section{Path: "xbc", Owner: "the xbc runtime", Kind: SectionTyped, Schema: reflect.TypeOf(frameworkSection{})},
		Section{Path: "web", Owner: "the web transport", Kind: SectionTyped, Schema: reflect.TypeOf(serverSection{})},
		Section{Path: "xbcx", Owner: "a different section", Kind: SectionTyped, Schema: reflect.TypeOf(serverSection{})},
	)
	require.NoError(t, err)
	return universe
}

// serverSection is the typed schema the environment-layer tests resolve
// against. It deliberately contains a key that itself holds an underscore
// (base_path) and a nested struct, because those are exactly the shapes a
// naive "split on underscore" environment convention gets wrong.
type serverSection struct {
	Addr     string        `yaml:"addr"`
	BasePath string        `yaml:"base_path"`
	Timeout  time.Duration `yaml:"timeout"`
	Pool     struct {
		MaxIdle int `yaml:"max_idle"`
	} `yaml:"pool"`
	Hosts []string       `yaml:"hosts"`
	Extra map[string]int `yaml:"extra"`
}

type storeSection struct {
	DSN      string `yaml:"dsn"`
	PoolSize int    `yaml:"pool_size"`
}

func testUniverse(t *testing.T) *Universe {
	t.Helper()
	universe, err := NewUniverse(
		Section{Path: "server", Owner: "the framework", Kind: SectionTyped, Schema: reflect.TypeOf(serverSection{})},
		Section{Path: "app", Owner: "the application", Kind: SectionFreeform},
		Section{Path: "plugins", Owner: "the assembly layer", Kind: SectionNamespace},
		Section{Path: "plugins.store", Owner: `plugin "store"`, Kind: SectionInstanced, Schema: reflect.TypeOf(storeSection{}), Toggle: true},
		Section{Path: "plugins.greeter", Owner: `plugin "greeter"`, Kind: SectionTyped, Toggle: true},
	)
	require.NoError(t, err)
	return universe
}

func TestUniverseRejectsTopLevelKeyNobodyOwns(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "application.yml"), "wbe:\n  addr: \":8080\"\n")

	_, _, err := loadKoanf(Options{Universe: testUniverse(t)})
	require.Error(t, err, "A misspelled top-level section must fail rather than be silently ignored")
	require.Contains(t, err.Error(), "wbe", "The error must name the offending path")
	require.Contains(t, err.Error(), "app, plugins, server", "The error must list what is actually declared")
}

func TestUniverseAcceptsDeclaredFreeformApplicationRoot(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "application.yml"), "app:\n  anything: {nested: 1}\n")

	k, _, err := loadKoanf(Options{Universe: testUniverse(t)})
	require.NoError(t, err, "A declared freeform root accepts keys the framework never interprets")
	require.Equal(t, 1, k.Int("app.anything.nested"))
}

// TestUniverseRejectsUnownedKeyBesideADeepSection is the reason ownership is a
// walk rather than a top-level scan. A Definition may rename its section to a
// dotted ConfigPath, and the segment it nests under is then claimed by nobody
// in particular -- so without descending into it, a sibling typo lands in a
// blind spot between the unowned-root check and strict bind, and is silently
// ignored.
func TestUniverseRejectsUnownedKeyBesideADeepSection(t *testing.T) {
	universe, err := NewUniverse(
		Section{Path: "plugins", Owner: "the assembly layer", Kind: SectionNamespace},
		Section{Path: "plugins.group.actual", Owner: `plugin "renamed"`, Kind: SectionTyped, Toggle: true},
	)
	require.NoError(t, err)

	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "application.yml"),
		"plugins:\n  group:\n    actual:\n      enabled: true\n    typo:\n      enabled: true\n")

	_, _, err = loadKoanf(Options{Universe: universe})
	require.Error(t, err, "A sibling of a renamed section is owned by nobody and must fail")
	require.Contains(t, err.Error(), "plugins.group.typo", "The error must name the full path, not just its root")
	require.Contains(t, err.Error(), "plugins.group.actual",
		"The error must name what is declared beside it, not the distant top-level roots")
}

// TestUniverseLeavesTheInteriorOfADeclaredSectionToBind draws the line the walk
// stops at. Descending past a declared section would make this check a second,
// weaker copy of strict bind -- and would reject legal keys, since an instance
// name and a freeform subtree are by definition unknown to any schema.
func TestUniverseLeavesTheInteriorOfADeclaredSectionToBind(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "application.yml"),
		"server:\n  pool:\n    max_idle: 4\n"+
			"app:\n  anything:\n    nested: 1\n"+
			"plugins:\n  store:\n    primary:\n      dsn: from-file\n")

	k, _, err := loadKoanf(Options{Universe: testUniverse(t)})
	require.NoError(t, err)
	require.Equal(t, 4, k.Int("server.pool.max_idle"), "a nested schema leaf is the owning section's business")
	require.Equal(t, "from-file", k.String("plugins.store.primary.dsn"),
		"an instance name is known only to the configuration, never to a schema")
}

func TestUniverseNestedSectionMustLiveInsideANamespace(t *testing.T) {
	_, err := NewUniverse(
		Section{Path: "server", Owner: "the framework", Kind: SectionTyped, Schema: reflect.TypeOf(serverSection{})},
		Section{Path: "server.store", Owner: `plugin "store"`, Kind: SectionTyped},
	)
	require.Error(t, err, "A plugin must not graft a custom ConfigPath onto a closed typed schema")
	require.Contains(t, err.Error(), "server.store")
}

// TestUniverseRejectsNestedSectionWithNoDeclaredRoot pins the other half of the
// grafting rule: nesting inside a *foreign* schema is rejected above, and
// nesting under a root nobody declared at all is rejected here. Without this a
// plugin could invent an entire top-level namespace by writing a dotted
// ConfigPath, and the unowned-root check would then wave its children through.
func TestUniverseRejectsNestedSectionWithNoDeclaredRoot(t *testing.T) {
	_, err := NewUniverse(
		Section{Path: "plugins", Owner: "the assembly layer", Kind: SectionNamespace},
		Section{Path: "infra.store.pool", Owner: `plugin "store"`, Kind: SectionTyped},
	)
	require.Error(t, err, "A nested section must descend from a declared namespace, not from thin air")
	require.Contains(t, err.Error(), "infra.store.pool", "The error must name the offending section")
	require.Contains(t, err.Error(), "infra", "The error must name the root that nobody declared")
}

func TestUniverseRejectsTwoOwnersForOnePath(t *testing.T) {
	_, err := NewUniverse(
		Section{Path: "plugins", Owner: "the assembly layer", Kind: SectionNamespace},
		Section{Path: "plugins.store", Owner: `plugin "a"`, Kind: SectionTyped},
		Section{Path: "plugins.store", Owner: `plugin "b"`, Kind: SectionTyped},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), `plugin "a"`)
	require.Contains(t, err.Error(), `plugin "b"`)
}

func TestEnvironmentLayerResolvesAgainstDeclaredSchema(t *testing.T) {
	universe := testUniverse(t)

	cases := []struct {
		name    string
		env     string
		value   string
		path    string
		want    any
		comment string
	}{
		{
			name:  "scalar leaf",
			env:   "XBC_SERVER_ADDR",
			value: ":9000",
			path:  "server.addr",
			want:  ":9000",
		},
		{
			name:    "leaf whose own key contains an underscore",
			env:     "XBC_SERVER_BASE_PATH",
			value:   "/api",
			path:    "server.base_path",
			want:    "/api",
			comment: "BASE_PATH must reach base_path, not server.base.path",
		},
		{
			name:  "nested leaf",
			env:   "XBC_SERVER_POOL_MAX_IDLE",
			value: "7",
			path:  "server.pool.max_idle",
			want:  7,
		},
		{
			name:  "duration leaf",
			env:   "XBC_SERVER_TIMEOUT",
			value: "3s",
			path:  "server.timeout",
			want:  3 * time.Second,
		},
		{
			name:  "string slice leaf",
			env:   "XBC_SERVER_HOSTS",
			value: "a,b",
			path:  "server.hosts",
			want:  []string{"a", "b"},
		},
		{
			name:    "single-instance plugin toggle",
			env:     "XBC_PLUGINS_GREETER_ENABLED",
			value:   "true",
			path:    "plugins.greeter.enabled",
			want:    true,
			comment: "The framework-owned enabled flag is addressable even without a schema",
		},
		{
			name:    "multi-instance leaf",
			env:     "XBC_PLUGINS_STORE_PRIMARY_POOL_SIZE",
			value:   "12",
			path:    "plugins.store.primary.pool_size",
			want:    12,
			comment: "The instance name is discovered from the variable, never from a schema",
		},
		{
			name:  "multi-instance toggle",
			env:   "XBC_PLUGINS_STORE_REPLICA_ENABLED",
			value: "false",
			path:  "plugins.store.replica.enabled",
			want:  false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			values, err := universe.envOverlay(DefaultEnvPrefix, []string{testCase.env + "=" + testCase.value}, nil)
			require.NoError(t, err, testCase.comment)
			require.Equal(t, testCase.want, values[testCase.path], testCase.comment)
			require.Len(t, values, 1, "A variable must set exactly the path it names")
		})
	}
}

func TestEnvironmentLayerRejectsShapesItCannotExpress(t *testing.T) {
	universe := testUniverse(t)

	cases := []struct {
		name    string
		entry   string
		wants   []string
		comment string
	}{
		{
			name:    "unclaimed prefix",
			entry:   "XBC_WBE_ADDR=x",
			wants:   []string{"XBC_WBE_ADDR", "app, plugins, server"},
			comment: "The reserved prefix behaves like a top-level key: an unowned name fails loudly",
		},
		{
			name:  "claimed section, unknown field",
			entry: "XBC_SERVER_NO_SUCH_FIELD=x",
			wants: []string{"names no configuration field"},
		},
		{
			name:    "map-valued leaf",
			entry:   "XBC_SERVER_EXTRA=x",
			wants:   []string{"cannot be carried by a single environment variable", "configure it in a file"},
			comment: "A shape with no single-variable spelling must say so, not be silently lost",
		},
		{
			name:    "multi-instance leaf with no instance segment",
			entry:   "XBC_PLUGINS_STORE_DSN=x",
			wants:   []string{"one section per instance", "XBC_PLUGINS_STORE_<INSTANCE>_DSN"},
			comment: "The error must spell out the convention that would work",
		},
		{
			name:    "freeform section",
			entry:   "XBC_APP_ANYTHING=x",
			wants:   []string{"freeform section the framework never interprets"},
			comment: "Without a schema there is nothing to resolve the name against",
		},
		{
			name:  "unparseable value",
			entry: "XBC_SERVER_POOL_MAX_IDLE=not-a-number",
			wants: []string{"XBC_SERVER_POOL_MAX_IDLE", "cannot be parsed as int"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := universe.envOverlay(DefaultEnvPrefix, []string{testCase.entry}, nil)
			require.Error(t, err, testCase.comment)
			for _, want := range testCase.wants {
				require.Contains(t, err.Error(), want, testCase.comment)
			}
		})
	}
}

func TestEnvironmentLayerReportsAmbiguityRatherThanGuessing(t *testing.T) {
	universe, err := NewUniverse(
		Section{Path: "plugins", Owner: "the assembly layer", Kind: SectionNamespace},
		Section{Path: "plugins.store", Owner: `plugin "store"`, Kind: SectionTyped, Schema: reflect.TypeOf(struct {
			A struct {
				B string `yaml:"b"`
			} `yaml:"a"`
			AB string `yaml:"a_b"`
		}{})},
	)
	require.NoError(t, err)

	_, err = universe.envOverlay(DefaultEnvPrefix, []string{"XBC_PLUGINS_STORE_A_B=x"}, nil)
	require.Error(t, err, "Two schema leaves share one environment spelling; picking one would be a guess")
	require.Contains(t, err.Error(), "plugins.store.a.b")
	require.Contains(t, err.Error(), "plugins.store.a_b")
}

func TestEnvironmentErrorsNeverEchoTheValue(t *testing.T) {
	universe := testUniverse(t)
	const secret = "super-secret-password"

	_, err := universe.envOverlay(DefaultEnvPrefix, []string{"XBC_SERVER_POOL_MAX_IDLE=" + secret}, nil)
	require.Error(t, err)
	require.NotContains(t, err.Error(), secret,
		"Environment variables are where secrets live; a parse failure is diagnosable from name and type alone")
}

func TestEnvironmentLayerLeavesTheProfileVariableAlone(t *testing.T) {
	universe := testUniverse(t)

	values, err := universe.envOverlay(DefaultEnvPrefix, []string{"XBC_PROFILE=prod"}, nil)
	require.NoError(t, err, "XBC_PROFILE addresses the loader itself, not a configuration section")
	require.Empty(t, values)
}

func TestConfigurationLayerPrecedence(t *testing.T) {
	cases := []struct {
		name     string
		file     string
		profile  string
		override string
		env      string
		want     string
	}{
		{name: "file only", file: ":file", want: ":file"},
		{name: "profile beats file", file: ":file", profile: ":profile", want: ":profile"},
		{name: "override beats profile", file: ":file", profile: ":profile", override: ":override", want: ":override"},
		{name: "env beats override", file: ":file", profile: ":profile", override: ":override", env: ":env", want: ":env"},
		{name: "env beats file with no profile", file: ":file", env: ":env", want: ":env"},
		{name: "env alone", env: ":env", want: ":env"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			if testCase.file != "" {
				writeYAML(t, filepath.Join(dir, "application.yml"), "server:\n  addr: \""+testCase.file+"\"\n")
			}
			if testCase.profile != "" {
				writeYAML(t, filepath.Join(dir, "application-prod.yml"), "server:\n  addr: \""+testCase.profile+"\"\n")
			}
			options := Options{Profile: "prod", EnvPrefix: DefaultEnvPrefix, Universe: testUniverse(t)}
			if testCase.override != "" {
				options.Overrides = map[string]any{"server.addr": testCase.override}
			}
			if testCase.env != "" {
				t.Setenv("XBC_SERVER_ADDR", testCase.env)
			}

			k, _, err := loadKoanf(options)
			require.NoError(t, err)
			require.Equal(t, testCase.want, k.String("server.addr"))
		})
	}
}

func TestEnvironmentAloneMakesASectionExist(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("XBC_PLUGINS_GREETER_ENABLED", "true")

	k, _, err := loadKoanf(Options{EnvPrefix: DefaultEnvPrefix, Universe: testUniverse(t)})
	require.NoError(t, err)
	require.True(t, k.Exists("plugins.greeter"),
		"A WhenConfigured plugin can only be activated by ENV alone if the ENV layer is part of the merged view")
	require.True(t, k.Bool("plugins.greeter.enabled"))
}

func TestEnvironmentAloneCanDeclareMultipleInstances(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("XBC_PLUGINS_STORE_PRIMARY_DSN", "primary-dsn")
	t.Setenv("XBC_PLUGINS_STORE_REPLICA_DSN", "replica-dsn")

	k, _, err := loadKoanf(Options{EnvPrefix: DefaultEnvPrefix, Universe: testUniverse(t)})
	require.NoError(t, err)
	require.Equal(t, "primary-dsn", k.String("plugins.store.primary.dsn"))
	require.Equal(t, "replica-dsn", k.String("plugins.store.replica.dsn"))

	instances := k.Get("plugins.store").(map[string]any)
	require.Len(t, instances, 2, "Instance names are discovered from the environment, with no file involved")
}

// TestEnvironmentCannotSilentlyForkAHyphenatedInstance pins the resolution of
// the one lossy corner of the environment round trip. A dash is legal in an
// instance name declared in a file, but envSegment maps a dash and an
// underscore onto the same environment spelling, so an override aimed at
// "my-db" arrives as "my_db". Creating a second instance is exactly the silent
// failure the environment layer exists to remove, so it is an error instead.
func TestEnvironmentCannotSilentlyForkAHyphenatedInstance(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "application.yml"),
		"plugins:\n  store:\n    my-db:\n      dsn: from-file\n")
	t.Setenv("XBC_PLUGINS_STORE_MY_DB_DSN", "from-env")

	_, _, err := loadKoanf(Options{EnvPrefix: DefaultEnvPrefix, Universe: testUniverse(t)})
	require.Error(t, err, "The override would have forked a second instance rather than replacing the first")
	require.Contains(t, err.Error(), "XBC_PLUGINS_STORE_MY_DB_DSN", "The error must name the variable")
	require.Contains(t, err.Error(), `"my-db"`, "The error must name the instance that cannot be reached")
	require.Contains(t, err.Error(), "plugins.store", "The error must name the section")
	require.Contains(t, err.Error(), "rename", "The error must say what would fix it")
	require.NotContains(t, err.Error(), "from-env", "An environment error never echoes the value")
}

// TestEnvironmentOverridesAnInstanceItCanSpell is the negative control for the
// test above: when the file-declared name has an exact environment spelling the
// variable must override it, not trip the collision check.
func TestEnvironmentOverridesAnInstanceItCanSpell(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "application.yml"),
		"plugins:\n  store:\n    my_db:\n      dsn: from-file\n      pool_size: 3\n")
	t.Setenv("XBC_PLUGINS_STORE_MY_DB_DSN", "from-env")

	k, _, err := loadKoanf(Options{EnvPrefix: DefaultEnvPrefix, Universe: testUniverse(t)})
	require.NoError(t, err)
	require.Equal(t, "from-env", k.String("plugins.store.my_db.dsn"))
	require.Equal(t, 3, k.Int("plugins.store.my_db.pool_size"), "The rest of the instance survives the override")

	instances := k.Get("plugins.store").(map[string]any)
	require.Len(t, instances, 1, "The override must not have forked a second instance")
}

func TestEnvironmentProvenanceNamesSourcesNotValues(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "application.yml"), "server:\n  addr: \":8080\"\n")
	t.Setenv("XBC_PLUGINS_GREETER_ENABLED", "true")

	env, err := Load(Options{Universe: testUniverse(t)})
	require.NoError(t, err)

	require.Equal(t, []string{"file application.yml", "env"}, env.Sources(),
		"Sources are reported lowest precedence first")
	require.Equal(t, []string{"file application.yml"}, env.OriginsUnder("server"))
	require.Equal(t, []string{"env"}, env.OriginsUnder("plugins.greeter"))
	require.Empty(t, env.OriginsUnder("app"), "A section nobody configured has no origin")
	require.Equal(t, []string{"app", "plugins", "server"}, env.Roots())
}

// TestEnvironmentLayerResolvesARootThatSpellsTheProcessPrefix pins the overlay
// half of the collapsed-root rule, which is the half that used to reject the
// name outright.
//
// A section whose root already spells the process prefix does not repeat it, so
// xbc.shutdown_timeout is XBC_SHUTDOWN_TIMEOUT. Deriving the name by
// concatenating the prefix with the complete path produced
// XBC_XBC_SHUTDOWN_TIMEOUT instead, and the overlay -- which stripped the
// prefix and then looked for a section called XBC_ -- matched neither spelling
// the way an operator would write it: the bare one failed as an unknown
// section, and only the doubled one resolved.
func TestEnvironmentLayerResolvesARootThatSpellsTheProcessPrefix(t *testing.T) {
	universe := xbcRootUniverse(t)

	cases := []struct {
		name    string
		env     string
		value   string
		path    string
		want    any
		comment string
	}{
		{
			name:    "framework root collapses",
			env:     "XBC_SHUTDOWN_TIMEOUT",
			value:   "11s",
			path:    "xbc.shutdown_timeout",
			want:    11 * time.Second,
			comment: "The spelling every operator reaches for is the one that must work",
		},
		{
			name:    "framework root leaf",
			env:     "XBC_INSTANCE_ID",
			value:   "host-7",
			path:    "xbc.instance_id",
			want:    "host-7",
			comment: "Every leaf of the root collapses, not just the first",
		},
		{
			name:    "ordinary root does not collapse",
			env:     "XBC_WEB_ADDR",
			value:   ":9090",
			path:    "web.addr",
			want:    ":9090",
			comment: "A root that does not spell the prefix keeps its complete spelling",
		},
		{
			name:    "near-miss root keeps its doubled spelling",
			env:     "XBC_XBCX_ADDR",
			value:   ":9091",
			path:    "xbcx.addr",
			want:    ":9091",
			comment: "xbcx merely begins with xbc; a bare string prefix test would collapse it too",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			values, err := universe.envOverlay(DefaultEnvPrefix, []string{testCase.env + "=" + testCase.value}, nil)
			require.NoError(t, err, testCase.comment)
			require.Equal(t, testCase.want, values[testCase.path], testCase.comment)
			require.Len(t, values, 1, "A variable must set exactly the path it names")
		})
	}
}

// TestEnvironmentLayerRejectsTheDoubledSpellingOfTheFrameworkRoot is the other
// direction: the name nobody writes must not keep working. Leaving
// XBC_XBC_SHUTDOWN_TIMEOUT accepted would preserve the very spelling the
// correction exists to remove, and would leave two names for one leaf.
//
// It fails as an unknown top-level name rather than as a missing field, which is
// the accurate diagnosis: after the collapse, XBC_XBC_ names no section either.
func TestEnvironmentLayerRejectsTheDoubledSpellingOfTheFrameworkRoot(t *testing.T) {
	universe := xbcRootUniverse(t)

	_, err := universe.envOverlay(DefaultEnvPrefix, []string{"XBC_XBC_SHUTDOWN_TIMEOUT=11s"}, nil)
	require.Error(t, err, "The doubled spelling is not the name of any configuration field")
	require.Contains(t, err.Error(), "XBC_XBC_SHUTDOWN_TIMEOUT")
	require.Contains(t, err.Error(), "uses the reserved XBC_ prefix but names no declared configuration section")
}

// TestEnvironmentLayerStillReportsAnUnknownRootByItsOwnMessage keeps the
// collapse from swallowing the most useful diagnostic in the layer.
//
// Because the framework's root collapses into the process prefix, every
// XBC_-prefixed variable now lands on that section's doorstep. If a collapsed
// root claimed a variable merely by carrying the prefix, a typo such as
// XBC_ADDR would be reported as a missing field of the framework's own section
// and the "names no declared configuration section" message -- the one that
// lists the roots an operator could have meant -- would become unreachable for
// every variable. A collapsed root therefore claims one only by matching it.
func TestEnvironmentLayerStillReportsAnUnknownRootByItsOwnMessage(t *testing.T) {
	universe := xbcRootUniverse(t)

	_, err := universe.envOverlay(DefaultEnvPrefix, []string{"XBC_ADDR=:1"}, nil)
	require.Error(t, err, "A top-level name nobody declares must fail rather than be silently ignored")
	require.Contains(t, err.Error(), "XBC_ADDR")
	require.Contains(t, err.Error(), "uses the reserved XBC_ prefix but names no declared configuration section")
	require.Contains(t, err.Error(), "web, xbc, xbcx",
		"The diagnostic must list the roots an operator could have meant")
}

// TestEnvironmentLayerKeepsClaimingForAnOrdinaryNamespace is the control that
// stops the rule above from being over-applied. A namespace whose root does not
// spell the process prefix still claims its whole prefix, so a typo inside it is
// reported as the missing field it is rather than as an unknown top-level name.
func TestEnvironmentLayerKeepsClaimingForAnOrdinaryNamespace(t *testing.T) {
	universe := testUniverse(t)

	_, err := universe.envOverlay(DefaultEnvPrefix, []string{"XBC_SERVER_NO_SUCH_FIELD=x"}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "names no configuration field")
	require.NotContains(t, err.Error(), "uses the reserved")
}

// TestLoadAndBindAgreeOnTheFrameworkRootName is the end-to-end pin: the
// environment layer must inject the value under xbc.shutdown_timeout and Bind
// must read that same variable off the process environment. Those are two
// independent derivations of one name, and a disagreement between them is
// invisible from either side alone -- the overlay would merge a value that Bind
// never picks up, leaving the struct at its default with no error anywhere.
func TestLoadAndBindAgreeOnTheFrameworkRootName(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "application.yml"), "xbc:\n  shutdown_timeout: 5s\n")
	t.Setenv("XBC_SHUTDOWN_TIMEOUT", "11s")

	env, err := Load(Options{Universe: xbcRootUniverse(t)})
	require.NoError(t, err)
	require.Equal(t, 11*time.Second, env.Get("xbc.shutdown_timeout"),
		"The environment layer is the highest-precedence layer and must resolve the collapsed name")

	var bound frameworkSection
	require.NoError(t, env.Bind("xbc", &bound))
	assert.Equal(t, 11*time.Second, bound.ShutdownTimeout,
		"Bind reads the process environment directly; it must derive the same name as the overlay")
}
