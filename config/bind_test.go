package config

import (
	"fmt"
	"net/netip"
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
	k, _, _, err := loadKoanf(Options{Overrides: map[string]any{"plugins.gorm.default.max_open_conn": 7}})
	require.NoError(t, err)
	t.Setenv("XBC_PLUGINS_GORM_DEFAULT_MAX_OPEN_CONN", "77")

	var cfg gormLikeConfig
	require.NoError(t, bind(k, "plugins.gorm.default", &cfg, "XBC_"))
	require.Equal(t, 77, cfg.MaxOpenConn, "When ENV and embedder Overrides collide on the same key, ENV wins")
}

// collectionsItem and collectionsConfig mimic a plugin whose ConfigSpec
// pre-fills a map and a struct slice, the shapes a user value must replace
// whole rather than merge into.
type collectionsItem struct {
	Name    string `yaml:"name"`
	Timeout int    `yaml:"timeout"`
}

type collectionsConfig struct {
	Queues map[string]int    `yaml:"queues"`
	Items  []collectionsItem `yaml:"items"`
}

// TestBindUserMapReplacesPreFilledDefaults pins replace semantics for maps.
// The merge mapstructure performs by default would keep pre-filled keys the
// user never mentioned -- the reason an asynq "queues: {critical: 6}" could
// not remove the default "default" queue.
func TestBindUserMapReplacesPreFilledDefaults(t *testing.T) {
	env, err := NewEnvironment(map[string]any{
		"service": map[string]any{
			"queues": map[string]any{"critical": 6},
		},
	}, "")
	require.NoError(t, err)

	config := collectionsConfig{Queues: map[string]int{"default": 1}}
	require.NoError(t, env.Bind("service", &config))

	assert.Equal(t, map[string]int{"critical": 6}, config.Queues,
		"a map written by the user replaces the pre-filled one; default keys must not survive the merge")
	assert.Equal(t, map[string]int{"critical": 6}, env.Get("service.queues"))
}

// TestBindEmptyUserMapReplacesPreFilledDefaults is the same rule at its
// boundary: an explicitly written empty map clears the pre-filled keys rather
// than being ignored because it has no entries to merge.
func TestBindEmptyUserMapReplacesPreFilledDefaults(t *testing.T) {
	env, err := NewEnvironment(map[string]any{
		"service": map[string]any{
			"queues": map[string]any{},
		},
	}, "")
	require.NoError(t, err)

	config := collectionsConfig{Queues: map[string]int{"default": 1}}
	require.NoError(t, env.Bind("service", &config))

	assert.Empty(t, config.Queues, "an empty user map is an intent to clear, not a no-op")
}

// TestBindUserStructSliceDoesNotInheritDefaultElements pins replace semantics
// for struct slices. Reusing a pre-filled element would leave fields the user
// never wrote -- the element is decoded into, not replaced -- so each decoded
// element must start from its zero value.
func TestBindUserStructSliceDoesNotInheritDefaultElements(t *testing.T) {
	env, err := NewEnvironment(map[string]any{
		"service": map[string]any{
			"items": []any{map[string]any{"name": "from-file"}},
		},
	}, "")
	require.NoError(t, err)

	config := collectionsConfig{Items: []collectionsItem{{Name: "default", Timeout: 30}}}
	require.NoError(t, env.Bind("service", &config))

	require.Len(t, config.Items, 1)
	assert.Equal(t, "from-file", config.Items[0].Name)
	assert.Zero(t, config.Items[0].Timeout,
		"a decoded element must be fresh, not a pre-filled default element with the user's fields written over it")
}

// TestBindCollectionsTheSourceOmitsKeepTheirPreFilledValue guards the other
// half of replace semantics: a collection is replaced only when the source
// actually supplies it, so a value the caller pre-filled and the configuration
// never mentions survives the bind.
func TestBindCollectionsTheSourceOmitsKeepTheirPreFilledValue(t *testing.T) {
	env, err := NewEnvironment(map[string]any{
		"service": map[string]any{
			"queues": map[string]any{"critical": 6},
		},
	}, "")
	require.NoError(t, err)

	config := collectionsConfig{Queues: map[string]int{"default": 1}, Items: []collectionsItem{{Name: "kept"}}}
	require.NoError(t, env.Bind("service", &config))

	assert.Equal(t, []collectionsItem{{Name: "kept"}}, config.Items,
		"a slice the source does not mention keeps the pre-filled value")
}

// TestBindRejectsFractionalFloatForIntegerField pins the strict scalar rule:
// mapstructure truncates a fractional float into an integer field, silently
// turning 2.9 into 2. A whole number in range is still accepted.
func TestBindRejectsFractionalFloatForIntegerField(t *testing.T) {
	env, err := NewEnvironment(map[string]any{
		"service": map[string]any{"max_open_conn": 2.9},
	}, "")
	require.NoError(t, err)

	var config gormLikeConfig
	err = env.Bind("service", &config)
	require.Error(t, err, "A fractional value must not be truncated into an integer field")
	assert.Contains(t, err.Error(), "whole number")
	assert.Zero(t, config.MaxOpenConn, "the rejected value must not be truncated into the field")

	whole, err := NewEnvironment(map[string]any{
		"service": map[string]any{"max_open_conn": 3.0},
	}, "")
	require.NoError(t, err)
	var accepted gormLikeConfig
	require.NoError(t, whole.Bind("service", &accepted), "a whole number that fits the field is not lossy")
	assert.Equal(t, 3, accepted.MaxOpenConn)
}

// TestBindRejectsScalarTypeCoercions pins the removals of WeaklyTypedInput:
// a bool offered for a string field, and a quoted token offered for an
// integer field, are configuration mistakes rather than values to coerce.
func TestBindRejectsScalarTypeCoercions(t *testing.T) {
	boolForString, err := NewEnvironment(map[string]any{
		"service": map[string]any{"dsn": true},
	}, "")
	require.NoError(t, err)
	var config gormLikeConfig
	require.Error(t, boolForString.Bind("service", &config),
		"true must not silently become the string \"1\"")

	for _, token := range []string{"0x10", "0755", "not-a-number"} {
		quoted, err := NewEnvironment(map[string]any{
			"service": map[string]any{"max_open_conn": token},
		}, "")
		require.NoError(t, err)
		var coerced gormLikeConfig
		require.Error(t, quoted.Bind("service", &coerced),
			"the quoted token %q must not be rewritten into an integer", token)
	}
}

// textSetting is the shape the runtime's xbc.runtime settings use: a
// string-kind type that parses its own syntax, so its spelling is meaningful
// and its parser owns every diagnostic for the key.
type textSetting string

func (v *textSetting) UnmarshalText(text []byte) error {
	*v = textSetting(text)
	return nil
}

type textSettingConfig struct {
	MaxProcs textSetting `yaml:"max_procs"`
	Label    string      `yaml:"label"`
}

// TestBindConvertsNumbersForTextUnmarshalingFieldsOnly pins the one integer
// conversion strict binding keeps. An unquoted YAML integer for a field whose
// type parses its own text is the spelling an operator wrote, so it reaches
// UnmarshalText as its decimal text; a plain string field still refuses the
// number rather than inventing a spelling nobody wrote.
func TestBindConvertsNumbersForTextUnmarshalingFieldsOnly(t *testing.T) {
	env, err := NewEnvironment(map[string]any{
		"service": map[string]any{"max_procs": 4},
	}, "")
	require.NoError(t, err)

	var config textSettingConfig
	require.NoError(t, env.Bind("service", &config))
	require.Equal(t, textSetting("4"), config.MaxProcs)

	plain, err := NewEnvironment(map[string]any{
		"service": map[string]any{"label": 4},
	}, "")
	require.NoError(t, err)
	var refused textSettingConfig
	require.Error(t, plain.Bind("service", &refused),
		"a plain string field must not silently accept a number")
}

// failingTextSetting is a string-kind TextUnmarshaler whose UnmarshalText
// always fails and echoes exactly the text it was given, the way a real
// decoder (netip.Addr's ParseAddr, for one) does. It exists so these
// regression tests can assert on the exact text handed to UnmarshalText
// without depending on another package's error wording.
type failingTextSetting string

func (f *failingTextSetting) UnmarshalText(text []byte) error {
	return fmt.Errorf("rejected %q", string(text))
}

type maskedAndPlainTextConfig struct {
	Secret failingTextSetting `yaml:"secret" mask:"true"`
	Plain  failingTextSetting `yaml:"plain"`
}

// TestBindFileSourcedTextUnmarshalerErrorNeverEchoesTheValue pins the fix for
// the leak the ENV-only TestBindEnvParseFailureNeverEchoesTheValue did not
// cover: a file-sourced value that fails a TextUnmarshaler reaches mapstructure's
// DecodeHook, not the ENV overlay's setScalar, so it needed its own
// redaction. The configured text must never appear in the bind error, for a
// field tagged mask:"true" and for a plain field alike -- bind has no way to
// know, hook-side, which secret a caller forgot to tag, so every bind/decode
// error stays value-free rather than relying on the field being tagged.
func TestBindFileSourcedTextUnmarshalerErrorNeverEchoesTheValue(t *testing.T) {
	const secret = "hunter2-super-secret-token"

	masked, err := NewEnvironment(map[string]any{
		"service": map[string]any{"secret": secret},
	}, "")
	require.NoError(t, err)
	var maskedCfg maskedAndPlainTextConfig
	err = masked.Bind("service", &maskedCfg)
	require.Error(t, err)
	require.NotContains(t, err.Error(), secret)
	require.Contains(t, err.Error(), "secret", "the field path stays in the error")

	plain, err := NewEnvironment(map[string]any{
		"service": map[string]any{"plain": secret},
	}, "")
	require.NoError(t, err)
	var plainCfg maskedAndPlainTextConfig
	err = plain.Bind("service", &plainCfg)
	require.Error(t, err)
	require.NotContains(t, err.Error(), secret,
		"an untagged field must not leak either -- bind cannot tell which field a caller forgot to mask")
	require.Contains(t, err.Error(), "plain")
}

// TestBindFileSourcedNetipAddrErrorNeverEchoesTheValue pins the same
// redaction against a real stdlib TextUnmarshaler rather than a test double:
// netip.Addr's ParseAddr quotes exactly the string it rejected, so a secret
// routed through a netip.Addr field (a credential embedded in a host:port,
// for example) must not surface via that path either.
func TestBindFileSourcedNetipAddrErrorNeverEchoesTheValue(t *testing.T) {
	type addrConfig struct {
		Addr netip.Addr `yaml:"addr" mask:"true"`
	}
	const secret = "not-an-ip-but-looks-like-a-secret-token"

	env, err := NewEnvironment(map[string]any{
		"service": map[string]any{"addr": secret},
	}, "")
	require.NoError(t, err)

	var cfg addrConfig
	err = env.Bind("service", &cfg)
	require.Error(t, err)
	require.NotContains(t, err.Error(), secret)
	require.Contains(t, err.Error(), "addr")
}

// TestBindFileSourcedNumericHookErrorNeverEchoesTheValue pins the leak the
// roadmap called out for textFromNumber: an unquoted YAML integer offered to
// a TextUnmarshaler field is converted to its decimal text before reaching
// UnmarshalText, so a failing UnmarshalText must not echo that number either.
func TestBindFileSourcedNumericHookErrorNeverEchoesTheValue(t *testing.T) {
	type numericConfig struct {
		Setting failingTextSetting `yaml:"setting" mask:"true"`
	}
	const secretNumber = 193564827

	env, err := NewEnvironment(map[string]any{
		"service": map[string]any{"setting": secretNumber},
	}, "")
	require.NoError(t, err)

	var cfg numericConfig
	err = env.Bind("service", &cfg)
	require.Error(t, err)
	require.NotContains(t, err.Error(), fmt.Sprint(secretNumber))
	require.Contains(t, err.Error(), "setting")
}

// TestBindRejectsNegativeIntoUnsignedWithoutEchoingTheValue pins the gap
// rejectFractionalFloat and the TextUnmarshaler hooks left open: a
// file-sourced negative integer bound to an unsigned field reached
// mapstructure's own int-to-uint conversion unexamined, and that conversion
// silently reinterprets the negative value as a huge unsigned one rather
// than erroring, so a mask:"true" field leaked its raw digits via the error
// mapstructure built around the result.
func TestBindRejectsNegativeIntoUnsignedWithoutEchoingTheValue(t *testing.T) {
	type maskedUint8 struct {
		U uint8 `yaml:"u" mask:"true"`
	}
	env, err := NewEnvironment(map[string]any{
		"service": map[string]any{"u": -123456},
	}, "")
	require.NoError(t, err)
	var cfg maskedUint8
	err = env.Bind("service", &cfg)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "123456", "the masked field's digits must never reach the error")
	require.Contains(t, err.Error(), "u", "the field path stays in the error")
	require.Zero(t, cfg.U)

	type plainUint struct {
		U uint `yaml:"u"`
	}
	envWide, err := NewEnvironment(map[string]any{
		"service": map[string]any{"u": -5551234},
	}, "")
	require.NoError(t, err)
	var wide plainUint
	err = envWide.Bind("service", &wide)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "5551234")
	require.Zero(t, wide.U)
}

// TestBindRejectsIntegerOverflowIntoNarrowerField pins the silent-truncation
// half of the same gap: an in-range-type mismatch (an int or uint source
// into a narrower int8/uint8 target) must be rejected rather than truncated
// through Go's own integer conversion.
func TestBindRejectsIntegerOverflowIntoNarrowerField(t *testing.T) {
	type int8Field struct {
		V int8 `yaml:"v"`
	}
	cases := []int{300, -99999}
	for _, value := range cases {
		env, err := NewEnvironment(map[string]any{"service": map[string]any{"v": value}}, "")
		require.NoError(t, err)
		var cfg int8Field
		err = env.Bind("service", &cfg)
		require.Error(t, err, "value %d must not be truncated into an int8 field", value)
		require.Contains(t, err.Error(), "whole number")
		require.Zero(t, cfg.V, "a rejected value must not be truncated into the field")
	}

	type uint8Field struct {
		V uint8 `yaml:"v"`
	}
	env, err := NewEnvironment(map[string]any{"service": map[string]any{"v": 99999}}, "")
	require.NoError(t, err)
	var cfg uint8Field
	err = env.Bind("service", &cfg)
	require.Error(t, err, "99999 must not be truncated into a uint8 field")
	require.Contains(t, err.Error(), "whole number")
	require.Zero(t, cfg.V)
}

// TestBindDefaultTagDoesNotOverwriteAPreFilledValue pins the precedence a
// ConfigSpec.Defaults value has over a struct default tag: the tag supplies a
// value only where the target is still its zero value.
func TestBindDefaultTagDoesNotOverwriteAPreFilledValue(t *testing.T) {
	k := koanf.New(".")

	prefilled := gormLikeConfig{MaxOpenConn: 42}
	require.NoError(t, bind(k, "", &prefilled, "XBC_"))
	require.Equal(t, 42, prefilled.MaxOpenConn,
		"a value the caller pre-filled is a decision the default tag must not overwrite")
	require.Equal(t, 5*time.Second, prefilled.ConnTimeout,
		"a field still at its zero value falls back to its default tag")

	zero := gormLikeConfig{}
	require.NoError(t, bind(k, "", &zero, "XBC_"))
	require.Equal(t, 10, zero.MaxOpenConn, "the zero value is what a default tag fills")
}
