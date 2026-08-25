// config_test.go
package xbc

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/internal/conf"
	"github.com/xbcio/xbc/log"
)

func TestServerConfigDefaults(t *testing.T) {
	c := &Config{k: koanf.New(".")}
	var sc ServerConfig
	require.NoError(t, c.Unmarshal("server", &sc))

	require.Equal(t, ":8080", sc.Addr)
	require.Equal(t, "/", sc.BasePath)
	require.Equal(t, 10*time.Second, sc.ReadTimeout)
	require.Equal(t, 30*time.Second, sc.WriteTimeout, "裁决 R10：write_timeout 是本计划补的字段，默认 30s")
	require.Equal(t, 30*time.Second, sc.ShutdownTimeout)
}

func TestConfigUnmarshalAppliesXBCEnvPrefix(t *testing.T) {
	t.Setenv("XBC_SERVER_ADDR", ":9999")
	c := &Config{k: koanf.New(".")}

	var sc ServerConfig
	require.NoError(t, c.Unmarshal("server", &sc))
	require.Equal(t, ":9999", sc.Addr)
}

func TestConfigUnmarshalOnUninitializedConfigIsError(t *testing.T) {
	var c *Config
	var sc ServerConfig
	err := c.Unmarshal("server", &sc)
	require.Error(t, err, "Config 还没被 loadConfig 初始化时调用 Unmarshal 必须报错，不能拿着 nil 的 koanf 去 panic")
}

func TestConfigGetExistsSub(t *testing.T) {
	k := koanf.New(".")
	require.NoError(t, k.Load(confmap.Provider(map[string]any{
		"app": map[string]any{"feature_x": true},
	}, ""), nil))
	c := &Config{k: k}

	require.True(t, c.Exists("app.feature_x"))
	require.Equal(t, true, c.Get("app.feature_x"))
	require.Equal(t, map[string]any{"feature_x": true}, c.Sub("app"))
}

func TestConfigSubOnAbsentPathReturnsNil(t *testing.T) {
	c := &Config{k: koanf.New(".")}
	require.Nil(t, c.Sub("does.not.exist"))
}

func TestConfigSubOnScalarPathReturnsNil(t *testing.T) {
	k := koanf.New(".")
	require.NoError(t, k.Load(confmap.Provider(map[string]any{"server": map[string]any{"addr": ":8080"}}, ""), nil))
	c := &Config{k: k}
	require.Nil(t, c.Sub("server.addr"), "标量路径不是 map，Sub 应返回 nil 而不是 panic")
}

func TestBindLogFromYAMLInitializesLogger(t *testing.T) {
	k := koanf.New(".")
	require.NoError(t, k.Load(confmap.Provider(map[string]any{
		"log": map[string]any{"level": "warn"},
	}, ""), nil))

	bound, err := bindLog(k, defaultEnvPrefix)
	require.NoError(t, err)
	require.Equal(t, "warn", bound.Level, "bindLog 要把绑定后的 log.Config 交回去，供 Config.Log 保存")
	require.False(t, log.L().Enabled(log.InfoLevel), "level: warn 生效后，Info 级别应当被关闭")
	require.True(t, log.L().Enabled(log.WarnLevel))
}

func TestLoadConfigWithoutFileUsesDefaults(t *testing.T) {
	a := &App{}
	require.NoError(t, a.loadConfig(conf.Options{EnvPrefix: defaultEnvPrefix}))
	require.NotNil(t, a.cfg, "loadConfig 必须把结果挂到 App.cfg 上，后续九个阶段全靠它")
	require.Equal(t, ":8080", a.cfg.Server.Addr, "没有配置文件时 server 段应当全走 default tag")
	require.Equal(t, 30*time.Second, a.cfg.Server.WriteTimeout)
	require.True(t, a.cfg.Exists("server.addr"), "Config.k 必须被填上，否则 Exists/Sub 全瞎")
}

func TestLoadConfigRejectsMissingExplicitFile(t *testing.T) {
	a := &App{}
	err := a.loadConfig(conf.Options{File: "不存在的.yml", EnvPrefix: defaultEnvPrefix})
	require.Error(t, err, "显式 --config 指到不存在的文件必须报错（裁决 R8）")
}

func TestLoadConfigReportsServerValidationError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "application.yml")
	require.NoError(t, os.WriteFile(path, []byte("server:\n  read_timeout: 不是时长\n"), 0o600))

	a := &App{}
	err := a.loadConfig(conf.Options{File: path, EnvPrefix: defaultEnvPrefix})
	require.Error(t, err, "server 段绑定失败要一路冒泡到 loadConfig 的调用方")
}

// TestLoadConfigSyncsLogSectionIntoK guards bindLog's syncBack call
// (config.go), which the Task 6 review's mutation M6 found unguarded: deleting
// those three lines left every test green. server and log must behave the
// same way here -- the first person to notice Exists("server.addr") is true
// while Exists("log.level") is false would reasonably assume it's a bug, not
// an intentional asymmetry, so the two sections are kept consistent on
// purpose and that consistency needs its own test.
func TestLoadConfigSyncsLogSectionIntoK(t *testing.T) {
	a := &App{}
	require.NoError(t, a.loadConfig(conf.Options{EnvPrefix: defaultEnvPrefix}))

	require.True(t, a.cfg.Exists("log.level"), "log 段全靠 default tag 撑起来时，回写也要覆盖 log，不能只覆盖 server")
	require.Equal(t, log.DefaultConfig().Level, a.cfg.Get("log.level"))

	// A nested leaf (one level under log), to confirm the sync walks the
	// whole subtree rather than stopping at log's own top-level fields.
	require.True(t, a.cfg.Exists("log.console.enabled"), "回写要覆盖整棵子树，不能止步于顶层字段")
	require.Equal(t, log.DefaultConfig().Console.Enabled, a.cfg.Get("log.console.enabled"))
}

// TestLoadConfigServerDurationReboundIsStable guards against the concern
// raised in the Task 6 report: syncBack writes a bound time.Duration field
// back into k as its underlying int64-nanoseconds value, and a later
// Config.Unmarshal of the same subtree re-parses whatever is in k. If that
// round trip drifted, ReadTimeout would not survive a second Bind.
func TestLoadConfigServerDurationReboundIsStable(t *testing.T) {
	a := &App{}
	require.NoError(t, a.loadConfig(conf.Options{EnvPrefix: defaultEnvPrefix}))
	require.Equal(t, 10*time.Second, a.cfg.Server.ReadTimeout)

	var second ServerConfig
	require.NoError(t, a.cfg.Unmarshal("server", &second))
	require.Equal(t, 10*time.Second, second.ReadTimeout,
		"syncBack 把 Duration 回写成纳秒整数后，再次 Bind 同一子树应仍解析回相同的 Duration")
}

// TestLoadConfigServerDurationEnvOverrideStaysStableWhileEnvSet guards ENV
// override idempotency: as long as XBC_SERVER_READ_TIMEOUT stays set, every
// repeated Config.Unmarshal("server", ...) re-runs Bind's ENV overlay branch
// and re-parses the same "42s" string straight from the environment, on top
// of whatever syncBack wrote into k. This is a real, worth-having guarantee,
// but it does NOT exercise the syncBack round trip through k -- see
// TestLoadConfigServerDurationEnvOverrideSurvivesRoundTripThroughK for the
// test that actually does (a prior version of this test claimed to cover
// that round trip in its own comment while actually only covering this one;
// the review's mutation -- syncBack writing time.Duration as d.Seconds()
// instead of its native int64-nanoseconds form -- proved it by staying green
// when it should have gone red).
func TestLoadConfigServerDurationEnvOverrideStaysStableWhileEnvSet(t *testing.T) {
	t.Setenv("XBC_SERVER_READ_TIMEOUT", "42s")

	a := &App{}
	require.NoError(t, a.loadConfig(conf.Options{EnvPrefix: defaultEnvPrefix}))
	require.Equal(t, 42*time.Second, a.cfg.Server.ReadTimeout)

	for i := 0; i < 3; i++ {
		var again ServerConfig
		require.NoError(t, a.cfg.Unmarshal("server", &again))
		require.Equal(t, 42*time.Second, again.ReadTimeout, "第 %d 次重复 Bind 应仍恒为 42s", i+1)
	}
}

// TestLoadConfigServerDurationEnvOverrideSurvivesRoundTripThroughK is the
// test that actually walks the chain "ENV value -> syncBack -> k -> Bind
// reads it back out of k": the ENV variable is unset right after loadConfig
// (t.Setenv's own cleanup still restores whatever it was before this test
// ran, once the test ends), so on the second Unmarshal there is no ENV value
// left to re-parse, and Bind's default step is skipped too because
// k.Exists("server.read_timeout") is now true from syncBack's write. The
// only place ReadTimeout can still come from is whatever syncBack put into
// k -- if that had been written out in any form other than the native
// int64-nanoseconds representation (e.g. seconds as a float64), this is the
// test that would catch the drift.
func TestLoadConfigServerDurationEnvOverrideSurvivesRoundTripThroughK(t *testing.T) {
	t.Setenv("XBC_SERVER_READ_TIMEOUT", "42s")

	a := &App{}
	require.NoError(t, a.loadConfig(conf.Options{EnvPrefix: defaultEnvPrefix}))
	require.Equal(t, 42*time.Second, a.cfg.Server.ReadTimeout)

	require.NoError(t, os.Unsetenv("XBC_SERVER_READ_TIMEOUT"))

	var second ServerConfig
	require.NoError(t, a.cfg.Unmarshal("server", &second))
	require.Equal(t, 42*time.Second, second.ReadTimeout,
		"ENV 已取消、default 因 k.Exists 为真被跳过，这个值只能来自 syncBack 写进 k 的内容")
}
