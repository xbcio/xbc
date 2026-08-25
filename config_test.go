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
