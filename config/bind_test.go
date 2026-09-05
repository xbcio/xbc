package config

import (
	"reflect"
	"testing"
	"time"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
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

func TestEnvName(t *testing.T) {
	require.Equal(t, "XBC_PLUGINS_GORM_DEFAULT_MAX_OPEN_CONN",
		envName("XBC_", "plugins.gorm.default.max_open_conn"))
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

func TestBindEnvBeatsOverridesOnSameKeyKnownLimitation(t *testing.T) {
	// Known structural limitation: by the time Bind runs, the file, profile,
	// and Load-stage Overrides have already been flattened into one koanf
	// tree, so Bind cannot tell which layer a given value originally came
	// from. The global priority is default < file < profile < ENV < flag,
	// but within the three-step algorithm the Bind contract specifies
	// (unmarshal -> ENV -> default), ENV always overrides whatever value is
	// already in the koanf tree, with no way to tell whether that value came
	// from flag Overrides. This test pins down the real behavior: on the
	// same key, ENV beats Overrides -- this is not a globally-true priority
	// rule, only a boundary of this one Bind step.
	t.Chdir(t.TempDir())
	k, _, err := loadKoanf(Options{Overrides: map[string]any{"plugins.gorm.default.max_open_conn": 7}})
	require.NoError(t, err)
	t.Setenv("XBC_PLUGINS_GORM_DEFAULT_MAX_OPEN_CONN", "77")

	var cfg gormLikeConfig
	require.NoError(t, bind(k, "plugins.gorm.default", &cfg, "XBC_"))
	require.Equal(t, 77, cfg.MaxOpenConn, "Known limitation: When ENV and flag overrides collide on the same key, ENV wins")
}
