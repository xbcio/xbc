package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gormLikeConfig mimics the shape of a real plugin Config, deliberately
// carrying a multi-word key to pin down ruling R2 (ENV overrides cannot
// rely on a literal `_` -> `.` replacement).
type gormLikeConfig struct {
	DSN         string        `yaml:"dsn"`
	MaxOpenConn int           `yaml:"max_open_conn" default:"10"`
	ConnTimeout time.Duration `yaml:"conn_timeout"  default:"5s"`
	Enabled     bool          `yaml:"enabled"        default:"true"`
}

type tlsLeaf struct {
	Enabled bool `yaml:"enabled" default:"false"`
}

type serverLeaf struct {
	Addr string  `yaml:"addr" default:":8080"`
	TLS  tlsLeaf `yaml:"tls"`
}

type nestedRoot struct {
	Server serverLeaf `yaml:"server"`
}

type badDefaultConfig struct {
	Timeout time.Duration `yaml:"timeout" default:"not-a-duration"`
}

type tagsConfig struct {
	Tags []string `yaml:"tags" default:"a,b,c"`
}

// koanfFrom builds a koanf tree directly from a nested map, bypassing the
// filesystem -- bind_test.go tests Bind, not Load, so there is no need to
// write a temporary YAML file just to grow a tree. An empty delim means
// data is already nested.
func koanfFrom(t *testing.T, data map[string]any) *koanf.Koanf {
	t.Helper()
	k := koanf.New(".")
	require.NoError(t, k.Load(confmap.Provider(data, ""), nil))
	return k
}

func TestBindOverlaysEnvForMultiWordKey(t *testing.T) {
	k := koanfFrom(t, map[string]any{
		"plugins": map[string]any{"gorm": map[string]any{"default": map[string]any{"dsn": "file-value"}}},
	})
	t.Setenv("XBC_PLUGINS_GORM_DEFAULT_MAX_OPEN_CONN", "42")

	var cfg gormLikeConfig
	require.NoError(t, bind(k, "plugins.gorm.default", &cfg, "XBC_"))

	require.Equal(t, 42, cfg.MaxOpenConn,
		"multi-word key max_open_conn must be hit via schema reverse lookup, literal _ -> . replacement would translate it into max.open.conn")
}

func TestBindOverlaysEnvForDuration(t *testing.T) {
	k := koanfFrom(t, map[string]any{
		"plugins": map[string]any{"gorm": map[string]any{"default": map[string]any{"dsn": "x"}}},
	})
	t.Setenv("XBC_PLUGINS_GORM_DEFAULT_CONN_TIMEOUT", "15s")

	var cfg gormLikeConfig
	require.NoError(t, bind(k, "plugins.gorm.default", &cfg, "XBC_"))
	require.Equal(t, 15*time.Second, cfg.ConnTimeout)
}

// TestBindEnvParseFailureNeverEchoesTheValue pins the redaction rule for the
// per-leaf ENV overlay. strconv errors quote the input they rejected, so
// wrapping one here would put a mistyped secret into a log line.
func TestBindEnvParseFailureNeverEchoesTheValue(t *testing.T) {
	k := koanfFrom(t, map[string]any{
		"plugins": map[string]any{"gorm": map[string]any{"default": map[string]any{"dsn": "x"}}},
	})
	const secret = "hunter2-not-a-number"
	t.Setenv("XBC_PLUGINS_GORM_DEFAULT_MAX_OPEN_CONN", secret)

	var cfg gormLikeConfig
	err := bind(k, "plugins.gorm.default", &cfg, "XBC_")
	require.Error(t, err)
	require.Contains(t, err.Error(), "XBC_PLUGINS_GORM_DEFAULT_MAX_OPEN_CONN")
	require.Contains(t, err.Error(), "int", "The expected type is what makes the error actionable")
	require.NotContains(t, err.Error(), secret)
}

func TestBindFillsDefaultsForUnsetleaves(t *testing.T) {
	k := koanfFrom(t, map[string]any{
		"plugins": map[string]any{"gorm": map[string]any{"default": map[string]any{"dsn": "x"}}},
	})

	var cfg gormLikeConfig
	require.NoError(t, bind(k, "plugins.gorm.default", &cfg, "XBC_"))

	require.Equal(t, 10, cfg.MaxOpenConn, "Neither file nor ENV is set, should fall back to default tag")
	require.Equal(t, 5*time.Second, cfg.ConnTimeout)
	require.True(t, cfg.Enabled)
}

func TestBindExplicitFalseIsNotOverriddenByDefaultTrue(t *testing.T) {
	k := koanfFrom(t, map[string]any{
		"plugins": map[string]any{"gorm": map[string]any{"default": map[string]any{
			"dsn":     "x",
			"enabled": false,
		}}},
	})

	var cfg gormLikeConfig
	require.NoError(t, bind(k, "plugins.gorm.default", &cfg, "XBC_"))

	require.False(t, cfg.Enabled,
		"explicitly written enabled: false in yml cannot be quietly changed by default:\"true\" — default must check if key has appeared, not whether field is zero value")
}

func TestBindDefaultParseFailureIsError(t *testing.T) {
	k := koanf.New(".")
	var cfg badDefaultConfig
	err := bind(k, "", &cfg, "XBC_")
	require.Error(t, err, "default tag itself is wrong, must expose it during binding phase, cannot delay to runtime crash")
}

func TestLeavesWalksNestedStruct(t *testing.T) {
	var cfg nestedRoot
	leaves := leaves("", &cfg)

	paths := make([]string, len(leaves))
	for i, l := range leaves {
		paths[i] = l.Path
	}
	require.Contains(t, paths, "server.addr")
	require.Contains(t, paths, "server.tls.enabled")
}

// TestEnvName pins the environment-variable spelling of a rooted configuration
// path, including the one root that collapses.
//
// The framework's own section is spelled "xbc" and the process prefix is
// "XBC_", so deriving the name by concatenating the two would produce
// XBC_XBC_SHUTDOWN_TIMEOUT -- a name no operator writes, and the reason
// XBC_SHUTDOWN_TIMEOUT used to be rejected as naming no section at all. Every
// other root keeps its complete spelling.
//
// The near-miss case is the guard against "fix" this with a bare string
// prefix test: "xbcx" merely begins with the same three characters as "xbc"
// and is a different section, so it keeps the doubled spelling.
func TestEnvName(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"plugins.gorm.default.max_open_conn": "XBC_PLUGINS_GORM_DEFAULT_MAX_OPEN_CONN",
		"xbc.shutdown_timeout":               "XBC_SHUTDOWN_TIMEOUT",
		"xbc.runtime.max_procs":              "XBC_RUNTIME_MAX_PROCS",
		"xbc.instance_id":                    "XBC_INSTANCE_ID",
		"web.addr":                           "XBC_WEB_ADDR",
		"plugins.redis.cache.password":       "XBC_PLUGINS_REDIS_CACHE_PASSWORD",
		"workloads.sast.enabled":             "XBC_WORKLOADS_SAST_ENABLED",
		"xbcx.foo":                           "XBC_XBCX_FOO",
	}
	for path, want := range cases {
		assert.Equal(t, want, envName("XBC_", path), "path %s", path)
	}
}

// TestEnvNameAgreesWithEnvSectionPrefix is the anti-drift guard between the two
// halves of the environment layer. bind reads a variable name straight off the
// process environment, while Universe resolves one by stripping a section's
// prefix and matching the remainder against that section's schema. If the two
// derivations disagree about a name, the overlay accepts a variable that bind
// never reads -- or rejects the one bind does -- and neither side can see the
// other's mistake.
//
// The remainder is taken from the leaf path rather than listed, so a case added
// here can only pass by the two derivations actually agreeing. A nested leaf
// such as xbc.runtime.max_procs belongs to its section with "runtime" still in
// the remainder, which is exactly how a nested sub-struct is addressed.
func TestEnvNameAgreesWithEnvSectionPrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		section string
		leaf    string
	}{
		{section: "xbc", leaf: "xbc.shutdown_timeout"},
		{section: "xbc", leaf: "xbc.runtime.max_procs"},
		{section: "xbc", leaf: "xbc.instance_id"},
		{section: "web", leaf: "web.addr"},
		{section: "plugins.gorm", leaf: "plugins.gorm.default.dsn"},
		{section: "plugins.redis.cache", leaf: "plugins.redis.cache.password"},
		{section: "workloads.sast", leaf: "workloads.sast.enabled"},
		{section: "xbcx", leaf: "xbcx.addr"},
	}
	for _, testCase := range cases {
		remainder := strings.TrimPrefix(testCase.leaf, testCase.section+".")
		require.NotEqual(t, testCase.leaf, remainder, "case %s must live under its section", testCase.leaf)
		assert.Equal(t,
			envName("XBC_", testCase.leaf),
			envSectionPrefix("XBC_", testCase.section)+envSegment(remainder),
			"section %s and leaf %s must spell one variable", testCase.section, testCase.leaf)
	}
}

func TestSetScalarParsesDurationNotAsInt(t *testing.T) {
	var d time.Duration
	v := reflect.ValueOf(&d).Elem()
	require.NoError(t, setScalar(v, v.Type(), "1h"))
	require.Equal(t, time.Hour, d,
		"time.Duration underlying is int64, reversed judgment order would parse 1h as integer and fail")
}

func TestSetScalarStringSliceSplitsOnComma(t *testing.T) {
	var ss []string
	v := reflect.ValueOf(&ss).Elem()
	require.NoError(t, setScalar(v, v.Type(), "a,b,c"))
	require.Equal(t, []string{"a", "b", "c"}, ss)
}

func TestSetScalarStringSliceSingleElementNoComma(t *testing.T) {
	var ss []string
	v := reflect.ValueOf(&ss).Elem()
	require.NoError(t, setScalar(v, v.Type(), "a"))
	require.Equal(t, []string{"a"}, ss)
}

func TestSetScalarStringSliceTrimsSurroundingSpaces(t *testing.T) {
	var ss []string
	v := reflect.ValueOf(&ss).Elem()
	require.NoError(t, setScalar(v, v.Type(), " a , b "))
	require.Equal(t, []string{"a", "b"}, ss,
		"Implementation trims whitespace for each comma-separated element, leading and trailing spaces should be removed")
}

func TestSetScalarStringSliceEmptyStringYieldsEmptySlice(t *testing.T) {
	var ss []string
	v := reflect.ValueOf(&ss).Elem()
	require.NoError(t, setScalar(v, v.Type(), ""))
	require.Equal(t, []string{}, ss,
		"Empty string is an explicit intent to clear list, must short-circuit into empty slice, not strings.Split(\"\", \",\") single-element [\"\"]")
	require.Len(t, ss, 0)
}

func TestPriorityChainDefaultFileProfileEnvOverrides(t *testing.T) {
	// The three tiers each use a different key, avoiding a collision with
	// the next "known limitation" test's key.
	k := koanfFrom(t, map[string]any{
		"plugins": map[string]any{"gorm": map[string]any{"default": map[string]any{
			"dsn": "from-file",
		}}},
	})
	t.Setenv("XBC_PLUGINS_GORM_DEFAULT_MAX_OPEN_CONN", "99")

	var cfg gormLikeConfig
	require.NoError(t, bind(k, "plugins.gorm.default", &cfg, "XBC_"))

	require.Equal(t, "from-file", cfg.DSN, "Fields set in file should take effect")
	require.Equal(t, 99, cfg.MaxOpenConn, "Fields set in ENV should override default")
	require.Equal(t, 5*time.Second, cfg.ConnTimeout, "Fields not set in either fall back to default")
}

func TestBindEnvEmptyStringClearsDefaultStringSlice(t *testing.T) {
	// Setting the ENV var itself (as opposed to leaving it unset) is how a
	// user explicitly overrides a default:"a,b,c" list down to zero
	// elements -- this exercises the ENV overlay stage of Bind, not just
	// setScalar in isolation, since the "does the default tag get skipped
	// once ENV has set this leaf" bookkeeping lives in Bind's envSet map.
	k := koanf.New(".")
	t.Setenv("XBC_TAGS", "")

	var cfg tagsConfig
	require.NoError(t, bind(k, "", &cfg, "XBC_"))

	require.Equal(t, []string{}, cfg.Tags,
		"Explicit empty string in ENV is intent to clear list, binding result should be empty slice, not fall back to default:\"a,b,c\"")
}

func TestBindEnvBeatsOverridesOnSameKey(t *testing.T) {
	// The documented priority is default < file < profile < Overrides < ENV,
	// with the environment on top, and this is the Bind step agreeing with
	// it. By the time Bind runs, the file, profile and Load-stage Overrides
	// have already been flattened into one koanf tree, so Bind cannot tell
	// which layer a value came from; its three-step algorithm (unmarshal ->
	// ENV -> default) simply lets the environment override whatever is
	// already there. That happens to produce exactly the global rule, so on
	// the same key ENV beats Overrides here as it does everywhere else.
	t.Chdir(t.TempDir())
	k, _, err := loadKoanf(Options{Overrides: map[string]any{"plugins.gorm.default.max_open_conn": 7}})
	require.NoError(t, err)
	t.Setenv("XBC_PLUGINS_GORM_DEFAULT_MAX_OPEN_CONN", "77")

	var cfg gormLikeConfig
	require.NoError(t, bind(k, "plugins.gorm.default", &cfg, "XBC_"))
	require.Equal(t, 77, cfg.MaxOpenConn, "When ENV and embedder Overrides collide on the same key, ENV wins")
}
