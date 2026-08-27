package container

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// This file ports the still-valid cases from the old root package's expand
// tests, rewritten around plugin.Definition/Activation
// instead of the retired source/entry/clonePrototype machinery. One thing
// from the old file is deliberately NOT ported, rather than silently
// dropped:
//
//   - TestExpandR7WarnsOnMultipleInstancesFromExplicitRegister and its
//     warnRecorder/warnCall/kvMap fixtures: ruling R7's warning existed only
//     because a Register call's prototype could carry constructor state
//     worth flagging as lost. Factory never carries constructor state to
//     begin with (see expand.go's expandMulti doc comment), so there is no
//     data loss left to warn about.
//
// TestExpandWiresContextAndBase, by contrast, IS ported (below) now that
// newInstance calls plugin.BindRuntimeContext: gap 1 of the plugin/container split
// closed the language-level barrier that used to make this test impossible
// to write from this package (plugin.bindBase and plugin.deriveName were
// unexported) -- see expand.go's newInstance doc comment.
//
// The other three R7-dependent tests (prototype reuse for a single
// registered instance, fresh zero value for a blank import, prototype
// skipped for a multi-instance expansion) are rewritten below as
// TestExpandBuildsFreshValueFromFactoryEveryTime and
// TestExpandMultiInstanceEachNamedInstanceGetsIndependentFactoryCall: since
// every instance now comes from calling Definition.Factory, the only thing
// left to pin down is that expand() never reuses any value across two
// independent Factory calls.

// fakeSinglePlugin is a minimal plugin value. It carries no runtime
// cardinality method; Definition.Instances owns that policy.
type fakeSinglePlugin struct{}

// fakeMultiPlugin is used by Definitions that statically declare MultipleInstances.
type fakeMultiPlugin struct{}

func TestExpandEnableRuleMatrix(t *testing.T) {
	cases := []struct {
		name       string
		activation plugin.Activation
		data       map[string]any // nil means the "plugins" namespace is entirely absent
		wantLen    int
	}{
		{
			"Always 且无配置节 → 启用",
			plugin.Always, nil, 1,
		},
		{
			"Always 且配置节存在 → 启用",
			plugin.Always,
			map[string]any{"plugins": map[string]any{"demo": map[string]any{"x": 1}}},
			1,
		},
		{
			"Always 且 enabled:false → 关闭",
			plugin.Always,
			map[string]any{"plugins": map[string]any{"demo": map[string]any{"enabled": false}}},
			0,
		},
		{
			"Configured(path) 且无配置节 → 不启用",
			plugin.Configured("plugins.demo"), nil, 0,
		},
		{
			"Configured(path) 且配置节存在（哪怕空） → 启用",
			plugin.Configured("plugins.demo"),
			map[string]any{"plugins": map[string]any{"demo": map[string]any{}}},
			1,
		},
		{
			"Configured(path) 且 enabled:false → 关闭",
			plugin.Configured("plugins.demo"),
			map[string]any{"plugins": map[string]any{"demo": map[string]any{"enabled": false}}},
			0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := plugin.Definition{
				Key:        "demo",
				Factory:    func() plugin.Plugin { return &fakeSinglePlugin{} },
				Activation: tc.activation,
			}
			snap := freezeDefs(t, def)
			c := newTestContainer(t, snap, tc.data)

			insts, _, err := c.expand()
			require.NoError(t, err)
			assert.Len(t, insts, tc.wantLen)
		})
	}
}

// TestExpandDisabledActivationNeverCallsFactory proves a Definition whose
// Activation gate is closed never calls Factory. Cardinality is static
// metadata, so no probe value is needed (see expand.go's doc comment).
func TestExpandDisabledActivationNeverCallsFactory(t *testing.T) {
	var calls int
	def := plugin.Definition{
		Key: "demo",
		Factory: func() plugin.Plugin {
			calls++
			return &fakeSinglePlugin{}
		},
		Activation: plugin.Configured("plugins.demo"),
	}
	snap := freezeDefs(t, def)
	c := newTestContainer(t, snap, nil) // no plugins.demo section at all

	insts, disabled, err := c.expand()
	require.NoError(t, err)
	assert.Empty(t, insts)
	assert.Equal(t, []string{"demo"}, disabled)
	assert.Equal(t, 0, calls, "Configured 路径的配置节缺失时，Factory 一次都不该被调用")
}

func TestExpandMultiInstanceExpandsNamedInstances(t *testing.T) {
	def := plugin.Definition{
		Key:        "gorm",
		Factory:    func() plugin.Plugin { return &fakeMultiPlugin{} },
		Instances:  plugin.MultipleInstances,
		Activation: plugin.Configured("plugins.gorm"),
	}
	snap := freezeDefs(t, def)
	c := newTestContainer(t, snap, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"default":  map[string]any{"dsn": "a"},
				"readonly": map[string]any{"dsn": "b"},
			},
		},
	})

	insts, _, err := c.expand()
	require.NoError(t, err)
	require.Len(t, insts, 2)

	names := map[string]bool{}
	for _, inst := range insts {
		names[inst.instance] = true
		assert.Equal(t, "gorm", inst.key.String())
	}
	assert.True(t, names["default"])
	assert.True(t, names["readonly"])
}

// TestExpandMultiInstanceMissingSectionDiffersByActivation is the new-world
// analog of the old ...ByRegistrationPath test: Always's "enabled unless
// explicitly turned off" and Configured(path)'s "enabled only if path
// exists" now play the role sourceRegister/sourceImport used to.
func TestExpandMultiInstanceMissingSectionDiffersByActivation(t *testing.T) {
	always := freezeDefs(t, plugin.Definition{
		Key:        "gorm",
		Factory:    func() plugin.Plugin { return &fakeMultiPlugin{} },
		Instances:  plugin.MultipleInstances,
		Activation: plugin.Always,
	})
	c := newTestContainer(t, always, nil)
	insts, _, err := c.expand()
	require.NoError(t, err)
	require.Len(t, insts, 1, "Always 且配置节缺失 → 展开出一个 default 实例")
	assert.Equal(t, defaultInstance, insts[0].instance)

	configured := freezeDefs(t, plugin.Definition{
		Key:        "gorm",
		Factory:    func() plugin.Plugin { return &fakeMultiPlugin{} },
		Instances:  plugin.MultipleInstances,
		Activation: plugin.Configured("plugins.gorm"),
	})
	c2 := newTestContainer(t, configured, nil)
	insts2, _, err := c2.expand()
	require.NoError(t, err)
	assert.Empty(t, insts2, "Configured(path) 且配置节缺失 → 不启用")
}

func TestExpandMultiInstanceSingleInstanceDisabled(t *testing.T) {
	var calls int
	def := plugin.Definition{
		Key: "gorm",
		Factory: func() plugin.Plugin {
			calls++
			return &fakeMultiPlugin{}
		},
		Instances:  plugin.MultipleInstances,
		Activation: plugin.Configured("plugins.gorm"),
	}
	snap := freezeDefs(t, def)
	c := newTestContainer(t, snap, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"default":  map[string]any{"dsn": "a"},
				"readonly": map[string]any{"dsn": "b", "enabled": false},
			},
		},
	})

	insts, _, err := c.expand()
	require.NoError(t, err)
	require.Len(t, insts, 1)
	assert.Equal(t, "default", insts[0].instance, "单独一个实例被 enabled:false 关掉，不影响别的实例")
	assert.Equal(t, 1, calls, "禁用实例不能调用 Factory；唯一一次调用只属于启用的 default 实例")
}

func TestExpandMultiInstancePluginLevelDisabledSkipsAll(t *testing.T) {
	var calls int
	def := plugin.Definition{
		Key: "gorm",
		Factory: func() plugin.Plugin {
			calls++
			return &fakeMultiPlugin{}
		},
		Instances:  plugin.MultipleInstances,
		Activation: plugin.Configured("plugins.gorm"),
	}
	snap := freezeDefs(t, def)
	c := newTestContainer(t, snap, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"enabled":  false,
				"default":  map[string]any{"dsn": "a"},
				"readonly": map[string]any{"dsn": "b"},
			},
		},
	})

	insts, _, err := c.expand()
	require.NoError(t, err)
	assert.Empty(t, insts, "插件级 enabled:false 必须关掉全部实例")
	assert.Equal(t, 0, calls, "插件级禁用必须在构造前短路，Factory 一次也不能调用")
}

func TestExpandMultiInstanceNonMapKeyErrors(t *testing.T) {
	def := plugin.Definition{
		Key:        "gorm",
		Factory:    func() plugin.Plugin { return &fakeMultiPlugin{} },
		Instances:  plugin.MultipleInstances,
		Activation: plugin.Configured("plugins.gorm"),
	}
	snap := freezeDefs(t, def)
	c := newTestContainer(t, snap, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"default":     map[string]any{"dsn": "a"},
				"max_retries": 3, // a scalar but not "enabled" -- invalid
			},
		},
	})

	_, _, err := c.expand()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `多实例插件 gorm 的配置节下 "max_retries" 不是实例（实例配置必须是映射）`)
}

// TestExpandMultiInstanceEmptyKeyErrors pins down that an empty-string
// instance key (plugins.gorm: {"": {...}}) is rejected at expansion time --
// almost always a config typo, not a deliberate "default instance" spelling.
func TestExpandMultiInstanceEmptyKeyErrors(t *testing.T) {
	def := plugin.Definition{
		Key:        "gorm",
		Factory:    func() plugin.Plugin { return &fakeMultiPlugin{} },
		Instances:  plugin.MultipleInstances,
		Activation: plugin.Configured("plugins.gorm"),
	}
	snap := freezeDefs(t, def)
	c := newTestContainer(t, snap, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"": map[string]any{"dsn": "a"},
			},
		},
	})

	_, _, err := c.expand()
	require.Error(t, err, "空字符串实例名必须在展开阶段就报错，不能悄悄归一化成 default")
	assert.Contains(t, err.Error(), "多实例插件 gorm 的实例名")
	assert.Contains(t, err.Error(), "实例名不能为空")
}

// TestExpandMultiInstanceReservedCharacterKeyErrors covers the other half:
// an instance name must also avoid every character the framework reserves
// for a plugin key -- '.' would corrupt the "plugins.<key>.<instance>" config
// path, and '[' / ']' would corrupt ID()'s "key[instance]"
// rendering.
func TestExpandMultiInstanceReservedCharacterKeyErrors(t *testing.T) {
	def := plugin.Definition{
		Key:        "gorm",
		Factory:    func() plugin.Plugin { return &fakeMultiPlugin{} },
		Instances:  plugin.MultipleInstances,
		Activation: plugin.Configured("plugins.gorm"),
	}
	snap := freezeDefs(t, def)
	c := newTestContainer(t, snap, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"read.only": map[string]any{"dsn": "a"},
			},
		},
	})

	_, _, err := c.expand()
	require.Error(t, err, "实例名含 '.' 会破坏 plugins.<name>.<instance> 配置路径，必须拒绝")
	assert.Contains(t, err.Error(), `"read.only"`)
	assert.Contains(t, err.Error(), "含非法字符")
}

func TestExpandOrphanSectionAbortsWithExactMessage(t *testing.T) {
	snap := freezeDefs(t) // empty snapshot -- nothing is a known plugin
	c := newTestContainer(t, snap, map[string]any{
		"plugins": map[string]any{
			"kafka": map[string]any{"brokers": "x"},
		},
	})

	_, _, err := c.expand()
	require.Error(t, err)
	assert.Equal(t,
		"xbc: plugins.kafka 有配置但无对应插件\n  → 是否忘了 import 对应的 provider/autoload 包？",
		err.Error())
}

func TestExpandOrphanSectionsAreAllListedAtOnce(t *testing.T) {
	snap := freezeDefs(t)
	c := newTestContainer(t, snap, map[string]any{
		"plugins": map[string]any{
			"kafka": map[string]any{"brokers": "x"},
			"mq":    map[string]any{"addr": "y"},
		},
	})

	_, _, err := c.expand()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugins.kafka 有配置但无对应插件", "多个孤儿节要一次全部列出，不能报一个就退")
	assert.Contains(t, err.Error(), "plugins.mq 有配置但无对应插件")
}

// TestExpandOrphanSectionsExcludesDisabledButKnownPlugins is the R6 positive
// test the task requires: a plugin present in the frozen Snapshot must never
// be treated as an orphan just because this run's configuration disabled
// it -- checkOrphanSections compares against every Definition name the
// Snapshot holds, not against the (possibly empty) set of instances expand()
// actually produced.
func TestExpandOrphanSectionsExcludesDisabledButKnownPlugins(t *testing.T) {
	def := plugin.Definition{
		Key:        "gorm",
		Factory:    func() plugin.Plugin { return &fakeSinglePlugin{} },
		Activation: plugin.Always,
	}
	snap := freezeDefs(t, def)
	c := newTestContainer(t, snap, map[string]any{
		"plugins": map[string]any{"gorm": map[string]any{"enabled": false}},
	})

	insts, disabled, err := c.expand()
	require.NoError(t, err, "gorm 出现在 Snapshot 里，哪怕这次运行被 enabled:false 关掉，也不该被判定为孤儿配置节")
	assert.Empty(t, insts)
	assert.Equal(t, []string{"gorm"}, disabled)
}

// TestExpandOrphanSectionRejectsTypoedPluginName is the R6 negative test:
// a config key that simply does not match any Definition name in the
// Snapshot (a typo, not a disabled-but-known plugin) must still be a fatal
// error.
func TestExpandOrphanSectionRejectsTypoedPluginKey(t *testing.T) {
	def := plugin.Definition{
		Key:        "gorm",
		Factory:    func() plugin.Plugin { return &fakeSinglePlugin{} },
		Activation: plugin.Always,
	}
	snap := freezeDefs(t, def)
	c := newTestContainer(t, snap, map[string]any{
		"plugins": map[string]any{"gorn": map[string]any{"dsn": "x"}}, // typo: "gorn", not "gorm"
	})

	_, _, err := c.expand()
	require.Error(t, err, "拼错的插件 key 不在 Snapshot 里，必须报孤儿配置节错误")
	assert.Contains(t, err.Error(), "plugins.gorn 有配置但无对应插件")
}

// TestExpandReservedNamespacesAreNeverTreatedAsOrphans confirms
// checkOrphanSections only ever inspects the "plugins" subtree: xbc./log./
// app. content sitting alongside it is simply invisible to this check,
// whether or not those namespaces are reserved elsewhere in the pipeline.
// server. is deliberately NOT among the reserved namespaces in this design,
// but that distinction is irrelevant here too -- checkOrphanSections never
// looked at top-level keys outside "plugins" to begin with.
func TestExpandReservedNamespacesAreNeverTreatedAsOrphans(t *testing.T) {
	snap := freezeDefs(t)
	c := newTestContainer(t, snap, map[string]any{
		"xbc":     map[string]any{"env": "dev"},
		"log":     map[string]any{"level": "info"},
		"app":     map[string]any{"feature_x": true},
		"plugins": map[string]any{},
	})

	insts, _, err := c.expand()
	require.NoError(t, err, "xbc/log/app 命名空间必须豁免于孤儿诊断")
	assert.Empty(t, insts)
}

// fakeCountedPlugin records which Factory call produced it, so a test can
// tell two independently-built values apart even though they are otherwise
// structurally identical.
type fakeCountedPlugin struct {
	instanceID int
}

// TestExpandBuildsFreshValueFromFactoryEveryTime replaces the old
// TestExpandR7ReusesPrototypeForSingleRegisteredInstance /
// TestExpandR7BuildsFreshZeroValueForBlankImport pair: there is no longer a
// "reuse the prototype" branch to distinguish from a "build fresh" branch --
// every instance, from every Container, always comes from its own Factory
// call. Two Containers built from the very same Snapshot must therefore
// still end up with two independently constructed plugin values.
func TestExpandBuildsFreshValueFromFactoryEveryTime(t *testing.T) {
	var next int
	factory := func() plugin.Plugin {
		next++
		return &fakeCountedPlugin{instanceID: next}
	}
	def := plugin.Definition{Key: "counted", Factory: factory, Activation: plugin.Always}
	snap := freezeDefs(t, def)

	c1 := newTestContainer(t, snap, nil)
	insts1, _, err := c1.expand()
	require.NoError(t, err)
	require.Len(t, insts1, 1)

	c2 := newTestContainer(t, snap, nil)
	insts2, _, err := c2.expand()
	require.NoError(t, err)
	require.Len(t, insts2, 1)

	p1 := insts1[0].plugin.(*fakeCountedPlugin)
	p2 := insts2[0].plugin.(*fakeCountedPlugin)
	assert.NotEqual(t, p1.instanceID, p2.instanceID,
		"每次 expand 都必须调用一次全新的 Factory，不能把值跨 Container 复用")
	assert.NotSame(t, p1, p2, "两个 Container 各自 expand 出来的插件值必须是不同的对象")
}

// fakeCountedMultiPlugin is fakeCountedPlugin's multi-instance twin.
type fakeCountedMultiPlugin struct {
	instanceID int
}

// TestExpandMultiInstanceEachNamedInstanceGetsIndependentFactoryCall proves
// that every enabled named instance gets exactly one independently constructed
// plugin value. There is no extra cardinality-probe value: cardinality comes
// only from Definition.Instances.
func TestExpandMultiInstanceEachNamedInstanceGetsIndependentFactoryCall(t *testing.T) {
	var next int
	factory := func() plugin.Plugin {
		next++
		return &fakeCountedMultiPlugin{instanceID: next}
	}
	def := plugin.Definition{Key: "gorm", Factory: factory, Instances: plugin.MultipleInstances, Activation: plugin.Always}
	snap := freezeDefs(t, def)
	c := newTestContainer(t, snap, map[string]any{
		"plugins": map[string]any{"gorm": map[string]any{
			"default":  map[string]any{},
			"readonly": map[string]any{},
		}},
	})

	insts, _, err := c.expand()
	require.NoError(t, err)
	require.Len(t, insts, 2)

	seen := map[int]bool{}
	for _, inst := range insts {
		p := inst.plugin.(*fakeCountedMultiPlugin)
		seen[p.instanceID] = true
	}
	assert.Len(t, seen, 2, "两个具名实例必须各自来自独立的 Factory 调用，不能共享同一个对象")
}

func TestExpandRejectsNilFactoryResult(t *testing.T) {
	def := plugin.Definition{
		Key:        "nil-result",
		Factory:    func() plugin.Plugin { return nil },
		Activation: plugin.Always,
	}
	c := newTestContainer(t, freezeDefs(t, def), nil)

	_, _, err := c.expand()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `插件 nil-result 的 Factory 为实例 "default" 返回了 nil`)
}

func TestExpandRejectsTypedNilFactoryResult(t *testing.T) {
	def := plugin.Definition{
		Key: "typed-nil-result",
		Factory: func() plugin.Plugin {
			var value *fakeSinglePlugin
			return value
		},
		Activation: plugin.Always,
	}
	c := newTestContainer(t, freezeDefs(t, def), nil)

	_, _, err := c.expand()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `插件 typed-nil-result 的 Factory 为实例 "default" 返回了 nil`)
}

func TestExpandRejectsNilUnsafePointerFactoryResult(t *testing.T) {
	def := plugin.Definition{
		Key: "typed-nil-unsafe-pointer",
		Factory: func() plugin.Plugin {
			var value unsafe.Pointer
			return value
		},
		Activation: plugin.Always,
	}
	c := newTestContainer(t, freezeDefs(t, def), nil)

	_, _, err := c.expand()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `插件 typed-nil-unsafe-pointer 的 Factory 为实例 "default" 返回了 nil`)
}

func TestInstanceIDAndLabel(t *testing.T) {
	single := &Instance{key: "cors", instance: defaultInstance, plugin: &fakeSinglePlugin{}}
	assert.Equal(t, "cors", single.ID())
	assert.Equal(t, "cors", single.Label(), "单实例插件的 default 实例，Label 与 ID 一致，不加 [default]")

	multiDefault := &Instance{key: "gorm", multiple: true, instance: defaultInstance, plugin: &fakeMultiPlugin{}}
	assert.Equal(t, "gorm", multiDefault.ID(), "ID() 里 default 被省略")
	assert.Equal(t, "gorm[default]", multiDefault.Label(), "Label() 要把多实例插件的 default 显式标出来")

	named := &Instance{key: "gorm", multiple: true, instance: "readonly", plugin: &fakeMultiPlugin{}}
	assert.Equal(t, "gorm[readonly]", named.ID())
	assert.Equal(t, "gorm[readonly]", named.Label())
}

func TestExpandMultiInstanceContextCarriesInstanceName(t *testing.T) {
	def := plugin.Definition{
		Key:        "gorm",
		Factory:    func() plugin.Plugin { return &fakeMultiPlugin{} },
		Instances:  plugin.MultipleInstances,
		Activation: plugin.Configured("plugins.gorm"),
	}
	snap := freezeDefs(t, def)
	c := newTestContainer(t, snap, map[string]any{
		"plugins": map[string]any{"gorm": map[string]any{"readonly": map[string]any{}}},
	})

	insts, _, err := c.expand()
	require.NoError(t, err)
	require.Len(t, insts, 1)
	assert.Equal(t, "readonly", insts[0].ctx.Instance())
	assert.Equal(t, "gorm", insts[0].ctx.Name())
}

// recordingLogger records every KV pair attached via With as a flat
// key/value slice, so a test can prove a plugin's Base.Log() returns this
// exact instance-scoped child logger rather than the global log.L()
// fallback (log.Nop() would satisfy a bare non-nil check too -- this fixture
// exists so the test has something to positively identify instead).
type recordingLogger struct {
	fields []any
}

func (l *recordingLogger) Debug(string, ...any) {}
func (l *recordingLogger) Info(string, ...any)  {}
func (l *recordingLogger) Warn(string, ...any)  {}
func (l *recordingLogger) Error(string, ...any) {}
func (l *recordingLogger) Fatal(string, ...any) {}
func (l *recordingLogger) With(kv ...any) log.Logger {
	return &recordingLogger{fields: append(append([]any(nil), l.fields...), kv...)}
}
func (l *recordingLogger) Enabled(log.Level) bool { return false }

// baseWiredPlugin embeds plugin.Base and deliberately does NOT define its
// own Name(): the promoted Base.Name() must be the one satisfying
// plugin.Plugin, so this fixture only compiles at all once BindRuntimeContext has a
// real name to hand it -- exactly the shape rule 1 (package-layout design
// §3.3) describes.
type baseWiredPlugin struct {
	plugin.Base
}

// TestExpandWiresContextAndBase is gap 1's pinned regression test: before
// newInstance called plugin.BindRuntimeContext, a plugin embedding plugin.Base saw
// an unbound Ctx() (nil), a Name() that stayed "", and a Log() that silently
// fell back to the global logger -- all three are asserted here with
// discriminating power, not bare non-nil checks.
func TestExpandWiresContextAndBase(t *testing.T) {
	logger := &recordingLogger{}
	def := plugin.Definition{
		Key:        "basewired",
		Factory:    func() plugin.Plugin { return &baseWiredPlugin{} },
		Activation: plugin.Always,
	}
	snap := freezeDefs(t, def)
	h := &fakeHost{}
	c := New(Options{
		Snapshot: snap,
		Env: newTestEnv(t, map[string]any{
			"plugins": map[string]any{"basewired": map[string]any{"own": "visible"}},
			"app":     map[string]any{"secret": "hidden"},
		}),
		Host:   h,
		Logger: logger,
	})
	h.c = c

	insts, _, err := c.expand()
	require.NoError(t, err)
	require.Len(t, insts, 1)

	p := insts[0].plugin.(*baseWiredPlugin)
	require.NotNil(t, p.Ctx(), "BindRuntimeContext 必须在 expand 阶段就把 Context 绑进 Base，不能留到 Init 才绑")
	assert.Same(t, insts[0].ctx, p.Ctx(), "Base.Ctx() 必须与 Instance 自己持有的 *plugin.Context 是同一个对象，不是另外重新构造的一个")
	assert.Equal(t, "basewired", p.Name(), "Base.Name() 绑定的必须是 Definition.Key，不是实例名，也不是插件自己的类型名")

	rl, ok := p.Log().(*recordingLogger)
	require.True(t, ok, "Base.Log() 必须返回真正绑定的实例 logger，不能悄悄回退成全局 log.L()")
	assert.Contains(t, rl.fields, "plugin", "实例 logger 必须带上 plugin 字段，证明它是 newInstance 派生出来的子 logger，不是凭空来的")
	assert.Contains(t, rl.fields, "basewired")
	assert.Equal(t, "visible", p.Ctx().Config().Get("own"),
		"Context.Config 的路径必须相对当前 Definition 的配置节")
	assert.Nil(t, p.Ctx().Config().Get("app.secret"),
		"实例作用域配置不能越权读取应用或其他 owner 的全局键")
}

// baseWiredMultiPlugin is baseWiredPlugin's multi-instance twin, used to
// confirm each named instance gets its own independently bound Context --
// not the same one shared across every instance of the plugin.
type baseWiredMultiPlugin struct {
	plugin.Base
}

func TestExpandMultiInstanceEachInstanceGetsIndependentContextAndBase(t *testing.T) {
	def := plugin.Definition{
		Key:        "gorm",
		Factory:    func() plugin.Plugin { return &baseWiredMultiPlugin{} },
		Instances:  plugin.MultipleInstances,
		Activation: plugin.Configured("plugins.gorm"),
	}
	snap := freezeDefs(t, def)
	c := newTestContainer(t, snap, map[string]any{
		"plugins": map[string]any{"gorm": map[string]any{
			"default":  map[string]any{"dsn": "primary"},
			"readonly": map[string]any{"dsn": "replica"},
		}},
	})

	insts, _, err := c.expand()
	require.NoError(t, err)
	require.Len(t, insts, 2)

	byInstance := make(map[string]*baseWiredMultiPlugin, 2)
	for _, inst := range insts {
		p := inst.plugin.(*baseWiredMultiPlugin)
		require.NotNil(t, p.Ctx(), "每个具名实例都必须各自被 BindRuntimeContext 绑定，不能只绑第一个")
		assert.Equal(t, "gorm", p.Name(), "Base.Name() 返回 Definition key 的文本，不随实例名变化")
		byInstance[inst.instance] = p
	}
	require.Len(t, byInstance, 2)
	assert.NotSame(t, byInstance["default"].Ctx(), byInstance["readonly"].Ctx(),
		"多实例插件的每个具名实例必须拿到各自独立的 *plugin.Context，不能共享同一个对象")
	assert.Equal(t, "default", byInstance["default"].Ctx().Instance())
	assert.Equal(t, "readonly", byInstance["readonly"].Ctx().Instance())
	assert.Equal(t, "primary", byInstance["default"].Ctx().Config().Get("dsn"))
	assert.Equal(t, "replica", byInstance["readonly"].Ctx().Config().Get("dsn"),
		"多实例 Context.Config 必须限定到当前实例，不能看到兄弟实例")
}
