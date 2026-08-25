package xbc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type demoConfig struct {
	DSN string `yaml:"dsn"           validate:"required"`
	Max int    `yaml:"max_open_conn" default:"5"`
}

type demoConfigurablePlugin struct {
	Base
	Cfg demoConfig
}

func (p *demoConfigurablePlugin) ConfigPtr() any { return &p.Cfg }

type multiConfigurablePlugin struct {
	Base
	Cfg demoConfig
}

func (p *multiConfigurablePlugin) MultiInstance() bool { return true }
func (p *multiConfigurablePlugin) ConfigPtr() any      { return &p.Cfg }

// noConfigPlugin does not implement Configurable at all.
type noConfigPlugin struct{ Base }

func TestBindConfigsSingleInstance(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{
			"demo": map[string]any{"dsn": "root:pwd@tcp(127.0.0.1:3306)/app"},
		},
	})}
	p := &demoConfigurablePlugin{}
	inst := &instance{plugin: p, name: "demo", instance: defaultInstance}

	require.NoError(t, a.bindConfigs([]*instance{inst}))
	assert.Equal(t, "root:pwd@tcp(127.0.0.1:3306)/app", p.Cfg.DSN)
	assert.Equal(t, 5, p.Cfg.Max, "default tag 在这一步生效")
}

func TestBindConfigsMultiInstanceIsolatesEachInstance(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"default":  map[string]any{"dsn": "a", "max_open_conn": 10},
				"readonly": map[string]any{"dsn": "b"},
			},
		},
	})}
	pDefault := &multiConfigurablePlugin{}
	pReadonly := &multiConfigurablePlugin{}
	insts := []*instance{
		{plugin: pDefault, name: "gorm", instance: "default"},
		{plugin: pReadonly, name: "gorm", instance: "readonly"},
	}

	require.NoError(t, a.bindConfigs(insts))
	assert.Equal(t, "a", pDefault.Cfg.DSN)
	assert.Equal(t, 10, pDefault.Cfg.Max)
	assert.Equal(t, "b", pReadonly.Cfg.DSN)
	assert.Equal(t, 5, pReadonly.Cfg.Max, "readonly 没写 max_open_conn，必须各自取默认值，互不影响")
}

func TestBindConfigsRequiredFieldMissingReportsPreciseInstancePath(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{"readonly": map[string]any{}},
		},
	})}
	inst := &instance{plugin: &multiConfigurablePlugin{}, name: "gorm", instance: "readonly"}

	err := a.bindConfigs([]*instance{inst})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugins.gorm.readonly.dsn",
		"错误必须精确指到具体实例的字段路径，不能只说 plugins.gorm")
}

func TestBindConfigsAggregatesErrorsAcrossPlugins(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{
			"demo": map[string]any{},
			"gorm": map[string]any{"readonly": map[string]any{}},
		},
	})}
	insts := []*instance{
		{plugin: &demoConfigurablePlugin{}, name: "demo", instance: defaultInstance},
		{plugin: &multiConfigurablePlugin{}, name: "gorm", instance: "readonly"},
	}

	err := a.bindConfigs(insts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugins.demo.dsn",
		"两个插件各错一条，必须一次全部报出，不能第一个错就退")
	assert.Contains(t, err.Error(), "plugins.gorm.readonly.dsn")
}

func TestBindConfigsSkipsPluginsWithoutConfigurable(t *testing.T) {
	a := &App{cfg: newTestConfig(t, nil)}
	inst := &instance{plugin: &noConfigPlugin{}, name: "plain", instance: defaultInstance}

	assert.NoError(t, a.bindConfigs([]*instance{inst}),
		"没实现 Configurable 的插件必须被跳过，不能因为找不到 ConfigPtr 报错")
}

func TestBindConfigsEnvOverride(t *testing.T) {
	t.Setenv("XBC_PLUGINS_DEMO_DSN", "env-dsn")
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{"demo": map[string]any{"dsn": "file-dsn"}},
	})}
	p := &demoConfigurablePlugin{}
	inst := &instance{plugin: p, name: "demo", instance: defaultInstance}

	require.NoError(t, a.bindConfigs([]*instance{inst}))
	assert.Equal(t, "env-dsn", p.Cfg.DSN, "ENV 覆盖必须在这一步生效，串起 Task 5 的 conf.Bind")
}

func TestBindConfigsIgnoresEnabledKeyAsPlainExtraField(t *testing.T) {
	// Stage 2 (expand, Task 8) is the one that interprets "enabled" and drops
	// disabled instances before stage 3 ever runs -- bindConfigs must never
	// see a disabled instance in insts. This test only proves the mechanical
	// side: an "enabled" key left in a config subtree does not break
	// unmarshalling, because mapstructure silently ignores fields the target
	// struct doesn't declare (verified against koanf v2.3.6 source above).
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{"demo": map[string]any{"dsn": "x", "enabled": true}},
	})}
	p := &demoConfigurablePlugin{}
	inst := &instance{plugin: p, name: "demo", instance: defaultInstance}

	require.NoError(t, a.bindConfigs([]*instance{inst}))
	assert.Equal(t, "x", p.Cfg.DSN)
}
