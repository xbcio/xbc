package xbc

import (
	"testing"

	"github.com/knadh/koanf/providers/confmap"
	koanf "github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestConfig builds a *Config directly from a nested map, bypassing file
// I/O entirely -- stage_expand tests only care about the shape of the config
// tree, not about how it got loaded.
func newTestConfig(t *testing.T, data map[string]any) *Config {
	t.Helper()
	k := koanf.New(".")
	if data != nil {
		// delim="" tells confmap the input is already nested, not flat --
		// passing "." here would make it try to re-split top-level keys.
		require.NoError(t, k.Load(confmap.Provider(data, ""), nil))
	}
	return &Config{k: k}
}

// fakeSinglePlugin is a minimal single-instance plugin: it does not implement
// MultiInstancer.
type fakeSinglePlugin struct {
	Base
}

// fakeMultiPlugin declares itself multi-instance.
type fakeMultiPlugin struct {
	Base
}

func (p *fakeMultiPlugin) MultiInstance() bool { return true }

func TestExpandEnableRuleMatrix(t *testing.T) {
	cases := []struct {
		name    string
		src     source
		data    map[string]any // nil means the "plugins" namespace is entirely absent
		wantLen int
	}{
		{
			"register 无配置节 → 启用",
			sourceRegister, nil, 1,
		},
		{
			"register 配置节存在 → 启用",
			sourceRegister,
			map[string]any{"plugins": map[string]any{"demo": map[string]any{"x": 1}}},
			1,
		},
		{
			"register enabled:false → 关闭",
			sourceRegister,
			map[string]any{"plugins": map[string]any{"demo": map[string]any{"enabled": false}}},
			0,
		},
		{
			"blank import 无配置节 → 不启用",
			sourceImport, nil, 0,
		},
		{
			"blank import 配置节存在（哪怕空）→ 启用",
			sourceImport,
			map[string]any{"plugins": map[string]any{"demo": map[string]any{}}},
			1,
		},
		{
			"blank import enabled:false → 关闭",
			sourceImport,
			map[string]any{"plugins": map[string]any{"demo": map[string]any{"enabled": false}}},
			0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &App{cfg: newTestConfig(t, tc.data)}
			a.entries = []entry{{proto: &fakeSinglePlugin{}, name: "demo", src: tc.src}}

			insts, err := a.expand()
			require.NoError(t, err)
			assert.Len(t, insts, tc.wantLen)
		})
	}
}

func TestExpandMultiInstanceExpandsNamedInstances(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"default":  map[string]any{"dsn": "a"},
				"readonly": map[string]any{"dsn": "b"},
			},
		},
	})}
	a.entries = []entry{{proto: &fakeMultiPlugin{}, name: "gorm", src: sourceImport, multi: true}}

	insts, err := a.expand()
	require.NoError(t, err)
	require.Len(t, insts, 2)

	names := map[string]bool{}
	for _, inst := range insts {
		names[inst.instance] = true
		assert.Equal(t, "gorm", inst.name)
	}
	assert.True(t, names["default"])
	assert.True(t, names["readonly"])
}

func TestExpandMultiInstanceMissingSectionDiffersByRegistrationPath(t *testing.T) {
	a := &App{cfg: newTestConfig(t, nil)}
	a.entries = []entry{{proto: &fakeMultiPlugin{}, name: "gorm", src: sourceRegister, multi: true}}
	insts, err := a.expand()
	require.NoError(t, err)
	require.Len(t, insts, 1, "显式 Register 且配置节缺失 → 展开出一个 default 实例")
	assert.Equal(t, defaultInstance, insts[0].instance)

	b := &App{cfg: newTestConfig(t, nil)}
	b.entries = []entry{{proto: &fakeMultiPlugin{}, name: "gorm", src: sourceImport, multi: true}}
	insts2, err := b.expand()
	require.NoError(t, err)
	assert.Empty(t, insts2, "blank import 且配置节缺失 → 不启用")
}

func TestExpandMultiInstanceSingleInstanceDisabled(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"default":  map[string]any{"dsn": "a"},
				"readonly": map[string]any{"dsn": "b", "enabled": false},
			},
		},
	})}
	a.entries = []entry{{proto: &fakeMultiPlugin{}, name: "gorm", src: sourceImport, multi: true}}

	insts, err := a.expand()
	require.NoError(t, err)
	require.Len(t, insts, 1)
	assert.Equal(t, "default", insts[0].instance, "单独一个实例被 enabled:false 关掉，不影响别的实例")
}

func TestExpandMultiInstancePluginLevelDisabledSkipsAll(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"enabled":  false,
				"default":  map[string]any{"dsn": "a"},
				"readonly": map[string]any{"dsn": "b"},
			},
		},
	})}
	a.entries = []entry{{proto: &fakeMultiPlugin{}, name: "gorm", src: sourceImport, multi: true}}

	insts, err := a.expand()
	require.NoError(t, err)
	assert.Empty(t, insts, "插件级 enabled:false 必须关掉全部实例")
}

func TestExpandMultiInstanceNonMapKeyErrors(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"default":     map[string]any{"dsn": "a"},
				"max_retries": 3, // a scalar but not "enabled" -- invalid
			},
		},
	})}
	a.entries = []entry{{proto: &fakeMultiPlugin{}, name: "gorm", src: sourceImport, multi: true}}

	_, err := a.expand()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `多实例插件 gorm 的配置节下 "max_retries" 不是实例（实例配置必须是映射）`)
}

func TestExpandOrphanSectionAbortsWithExactMessage(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{
			"kafka": map[string]any{"brokers": "x"},
		},
	})}

	_, err := a.expand()
	require.Error(t, err)
	assert.Equal(t,
		"xbc: plugins.kafka 有配置但无对应插件\n  → 是否忘了 import github.com/xbcio/xbc/plugins/kafka？",
		err.Error())
}

func TestExpandOrphanSectionsAreAllListedAtOnce(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{
			"kafka": map[string]any{"brokers": "x"},
			"mq":    map[string]any{"addr": "y"},
		},
	})}

	_, err := a.expand()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugins.kafka 有配置但无对应插件", "多个孤儿节要一次全部列出，不能报一个就退")
	assert.Contains(t, err.Error(), "plugins.mq 有配置但无对应插件")
}

func TestExpandReservedNamespacesAreNeverTreatedAsOrphans(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"server":  map[string]any{"addr": ":8080"},
		"log":     map[string]any{"level": "info"},
		"app":     map[string]any{"feature_x": true},
		"plugins": map[string]any{},
	})}

	insts, err := a.expand()
	require.NoError(t, err, "server/log/app 三个命名空间必须豁免于孤儿诊断")
	assert.Empty(t, insts)
}

func TestExpandDuplicatePluginNameErrors(t *testing.T) {
	a := &App{cfg: newTestConfig(t, nil)}
	a.entries = []entry{
		{proto: &fakeSinglePlugin{}, name: "dup", src: sourceRegister},
		{proto: &fakeSinglePlugin{}, name: "dup", src: sourceRegister},
	}
	_, err := a.expand()
	require.Error(t, err, "重复插件名必须报错，否则会在图节点 id 上悄悄合并成同一个节点")
	assert.Contains(t, err.Error(), "dup")
}

func TestExpandR7ReusesPrototypeForSingleRegisteredInstance(t *testing.T) {
	proto := &fakeSinglePlugin{}
	a := &App{cfg: newTestConfig(t, nil)}
	a.entries = []entry{{proto: proto, name: "cors", src: sourceRegister}}

	insts, err := a.expand()
	require.NoError(t, err)
	require.Len(t, insts, 1)
	assert.Same(t, proto, insts[0].plugin, "显式 Register 且只展开 1 个实例必须复用原型本身，保留构造参数")
}

func TestExpandR7BuildsFreshZeroValueForBlankImport(t *testing.T) {
	proto := &fakeSinglePlugin{}
	a := &App{cfg: newTestConfig(t, map[string]any{"plugins": map[string]any{"cors": map[string]any{}}})}
	a.entries = []entry{{proto: proto, name: "cors", src: sourceImport}}

	insts, err := a.expand()
	require.NoError(t, err)
	require.Len(t, insts, 1)
	assert.NotSame(t, proto, insts[0].plugin, "blank import 必须新建零值，不能复用包级 Register 时构造的那个原型")
}

func TestExpandR7MultiInstanceRegisteredWithMultipleInstancesSkipsPrototype(t *testing.T) {
	proto := &fakeMultiPlugin{}
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"default":  map[string]any{},
				"readonly": map[string]any{},
			},
		},
	})}
	a.entries = []entry{{proto: proto, name: "gorm", src: sourceRegister, multi: true}}

	insts, err := a.expand()
	require.NoError(t, err)
	require.Len(t, insts, 2)
	for _, inst := range insts {
		assert.NotSame(t, proto, inst.plugin,
			"展开出 >1 个实例时任何一个实例都不能复用原型——构造参数没法同时分给两份")
	}
}

func TestInstanceIDAndLabel(t *testing.T) {
	single := &instance{name: "cors", instance: defaultInstance, plugin: &fakeSinglePlugin{}}
	assert.Equal(t, "cors", single.id())
	assert.Equal(t, "cors", single.label(), "单实例插件的 default 实例，label 与 id 一致，不加 [default]")

	multiDefault := &instance{name: "gorm", instance: defaultInstance, plugin: &fakeMultiPlugin{}}
	assert.Equal(t, "gorm", multiDefault.id(), "id() 里 default 被省略")
	assert.Equal(t, "gorm[default]", multiDefault.label(), "label() 要把多实例插件的 default 显式标出来")

	named := &instance{name: "gorm", instance: "readonly", plugin: &fakeMultiPlugin{}}
	assert.Equal(t, "gorm[readonly]", named.id())
	assert.Equal(t, "gorm[readonly]", named.label())
}

func TestExpandWiresContextAndBase(t *testing.T) {
	a := &App{cfg: newTestConfig(t, nil)}
	a.entries = []entry{{proto: &fakeSinglePlugin{}, name: "cors", src: sourceRegister}}

	insts, err := a.expand()
	require.NoError(t, err)
	require.Len(t, insts, 1)

	p := insts[0].plugin.(*fakeSinglePlugin)
	assert.Equal(t, "cors", p.Name(), "bindBase 必须把框架推导的名字回填进 Base")
	assert.NotNil(t, p.Ctx(), "bindBase 必须把 Context 回填进 Base")
}

func TestExpandMultiInstanceContextCarriesInstanceName(t *testing.T) {
	a := &App{cfg: newTestConfig(t, map[string]any{
		"plugins": map[string]any{"gorm": map[string]any{"readonly": map[string]any{}}},
	})}
	a.entries = []entry{{proto: &fakeMultiPlugin{}, name: "gorm", src: sourceImport, multi: true}}

	insts, err := a.expand()
	require.NoError(t, err)
	require.Len(t, insts, 1)
	assert.Equal(t, "readonly", insts[0].ctx.Instance())
	assert.Equal(t, "gorm", insts[0].ctx.Name())
}
