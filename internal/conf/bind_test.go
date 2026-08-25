package conf

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
	require.NoError(t, Bind(k, "plugins.gorm.default", &cfg, "XBC_"))

	require.Equal(t, 42, cfg.MaxOpenConn,
		"多词 key max_open_conn 必须靠 schema 反查命中，字面 _ -> . 替换会把它错译成 max.open.conn")
}

func TestBindOverlaysEnvForDuration(t *testing.T) {
	k := koanfFrom(t, map[string]any{
		"plugins": map[string]any{"gorm": map[string]any{"default": map[string]any{"dsn": "x"}}},
	})
	t.Setenv("XBC_PLUGINS_GORM_DEFAULT_CONN_TIMEOUT", "15s")

	var cfg gormLikeConfig
	require.NoError(t, Bind(k, "plugins.gorm.default", &cfg, "XBC_"))
	require.Equal(t, 15*time.Second, cfg.ConnTimeout)
}

func TestBindFillsDefaultsForUnsetLeaves(t *testing.T) {
	k := koanfFrom(t, map[string]any{
		"plugins": map[string]any{"gorm": map[string]any{"default": map[string]any{"dsn": "x"}}},
	})

	var cfg gormLikeConfig
	require.NoError(t, Bind(k, "plugins.gorm.default", &cfg, "XBC_"))

	require.Equal(t, 10, cfg.MaxOpenConn, "文件和 ENV 都没设，应当落到 default tag")
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
	require.NoError(t, Bind(k, "plugins.gorm.default", &cfg, "XBC_"))

	require.False(t, cfg.Enabled,
		"yml 里显式写的 enabled: false 不能被 default:\"true\" 悄悄改掉——补 default 必须判 key 是否出现过，不能判字段是否为零值")
}

func TestBindDefaultParseFailureIsError(t *testing.T) {
	k := koanf.New(".")
	var cfg badDefaultConfig
	err := Bind(k, "", &cfg, "XBC_")
	require.Error(t, err, "default tag 本身写错了，必须在绑定阶段就暴露，不能拖到运行期才炸")
}

func TestLeavesWalksNestedStruct(t *testing.T) {
	var cfg nestedRoot
	leaves := Leaves("", &cfg)

	paths := make([]string, len(leaves))
	for i, l := range leaves {
		paths[i] = l.Path
	}
	require.Contains(t, paths, "server.addr")
	require.Contains(t, paths, "server.tls.enabled")
}

func TestEnvName(t *testing.T) {
	require.Equal(t, "XBC_PLUGINS_GORM_DEFAULT_MAX_OPEN_CONN",
		EnvName("XBC_", "plugins.gorm.default.max_open_conn"))
}

func TestSetScalarParsesDurationNotAsInt(t *testing.T) {
	var d time.Duration
	v := reflect.ValueOf(&d).Elem()
	require.NoError(t, setScalar(v, v.Type(), "1h"))
	require.Equal(t, time.Hour, d,
		"time.Duration 底层是 int64，判断顺序反了会把 1h 当整数解析失败")
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
		"实现对每个逗号分隔的元素都做了 strings.TrimSpace，前后空格应被去掉")
}

func TestSetScalarStringSliceEmptyStringYieldsEmptySlice(t *testing.T) {
	var ss []string
	v := reflect.ValueOf(&ss).Elem()
	require.NoError(t, setScalar(v, v.Type(), ""))
	require.Equal(t, []string{}, ss,
		"空串是显式清空列表的意图，必须短路成空切片，不能是 strings.Split(\"\", \",\") 的单元素 [\"\"]")
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
	require.NoError(t, Bind(k, "plugins.gorm.default", &cfg, "XBC_"))

	require.Equal(t, "from-file", cfg.DSN, "文件设置的字段应该生效")
	require.Equal(t, 99, cfg.MaxOpenConn, "ENV 设置的字段应该覆盖 default")
	require.Equal(t, 5*time.Second, cfg.ConnTimeout, "两者都没设的字段落到 default")
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
	require.NoError(t, Bind(k, "", &cfg, "XBC_"))

	require.Equal(t, []string{}, cfg.Tags,
		"ENV 显式设为空串是清空列表的意图，绑定结果应是空切片，而不是落回 default:\"a,b,c\"")
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
	k, err := Load(Options{Overrides: map[string]any{"plugins.gorm.default.max_open_conn": 7}})
	require.NoError(t, err)
	t.Setenv("XBC_PLUGINS_GORM_DEFAULT_MAX_OPEN_CONN", "77")

	var cfg gormLikeConfig
	require.NoError(t, Bind(k, "plugins.gorm.default", &cfg, "XBC_"))
	require.Equal(t, 77, cfg.MaxOpenConn, "已知限制：ENV 与 flag Overrides 撞在同一个 key 上时，ENV 赢")
}
