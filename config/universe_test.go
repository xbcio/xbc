package config

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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

func TestUniverseNestedSectionMustLiveInsideANamespace(t *testing.T) {
	_, err := NewUniverse(
		Section{Path: "server", Owner: "the framework", Kind: SectionTyped, Schema: reflect.TypeOf(serverSection{})},
		Section{Path: "server.store", Owner: `plugin "store"`, Kind: SectionTyped},
	)
	require.Error(t, err, "A plugin must not graft a custom ConfigPath onto a closed typed schema")
	require.Contains(t, err.Error(), "server.store")
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
			values, _, err := universe.envOverlay(DefaultEnvPrefix, []string{testCase.env + "=" + testCase.value})
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
			_, _, err := universe.envOverlay(DefaultEnvPrefix, []string{testCase.entry})
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

	_, _, err = universe.envOverlay(DefaultEnvPrefix, []string{"XBC_PLUGINS_STORE_A_B=x"})
	require.Error(t, err, "Two schema leaves share one environment spelling; picking one would be a guess")
	require.Contains(t, err.Error(), "plugins.store.a.b")
	require.Contains(t, err.Error(), "plugins.store.a_b")
}

func TestEnvironmentErrorsNeverEchoTheValue(t *testing.T) {
	universe := testUniverse(t)
	const secret = "super-secret-password"

	_, _, err := universe.envOverlay(DefaultEnvPrefix, []string{"XBC_SERVER_POOL_MAX_IDLE=" + secret})
	require.Error(t, err)
	require.NotContains(t, err.Error(), secret,
		"Environment variables are where secrets live; a parse failure is diagnosable from name and type alone")
}

func TestEnvironmentLayerLeavesTheProfileVariableAlone(t *testing.T) {
	universe := testUniverse(t)

	values, _, err := universe.envOverlay(DefaultEnvPrefix, []string{"XBC_PROFILE=prod"})
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
