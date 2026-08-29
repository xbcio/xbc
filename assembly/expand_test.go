package assembly

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
// newInstance calls plugin.BindRuntimeContext: gap 1 of the plugin/assembly split
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
			"Always and no configuration section → enabled",
			plugin.Always, nil, 1,
		},
		{
			"Always and configuration section exists → enabled",
			plugin.Always,
			map[string]any{"plugins": map[string]any{"demo": map[string]any{"x": 1}}},
			1,
		},
		{
			"Always and enabled:false → disabled",
			plugin.Always,
			map[string]any{"plugins": map[string]any{"demo": map[string]any{"enabled": false}}},
			0,
		},
		{
			"Configured(path) and no configuration section → not enabled",
			plugin.Configured("plugins.demo"), nil, 0,
		},
		{
			"Configured(path) and configuration section exists (even if empty) → enabled",
			plugin.Configured("plugins.demo"),
			map[string]any{"plugins": map[string]any{"demo": map[string]any{}}},
			1,
		},
		{
			"Configured(path) and enabled:false → disabled",
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
	assert.Equal(t, 0, calls, "Factory must never be called if the configuration section for Configured path is missing")
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
	require.Len(t, insts, 1, "Always and configuration section missing → expand a default instance")
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
	assert.Empty(t, insts2, "Configured(path) and configuration section missing → not enabled")
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
	assert.Equal(t, "default", insts[0].instance, "disabling a single instance does not affect other instances")
	assert.Equal(t, 1, calls, "disabled instances cannot call Factory; the only call belongs to the enabled default instance")
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
	assert.Empty(t, insts, "plugin-level enabled:false must disable all instances")
	assert.Equal(t, 0, calls, "plugin-level disable must short-circuit before construction, Factory must never be called")
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
	assert.Contains(t, err.Error(), `multi-instance plugin gorm's configuration section "max_retries" is not an instance (instance configuration must be a map)`)
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
	require.Error(t, err, "empty instance name must error during expansion phase, cannot silently normalize to default")
	assert.Contains(t, err.Error(), "multi-instance plugin gorm's instance name")
	assert.Contains(t, err.Error(), "instance name cannot be empty")
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
	require.Error(t, err, "instance name containing '.' breaks the plugins.<name>.<instance> configuration path, must be rejected")
	assert.Contains(t, err.Error(), `"read.only"`)
	assert.Contains(t, err.Error(), "contains invalid character")
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
		"xbc: plugins.kafka has configuration but no corresponding plugin\n  → Did you forget to import the corresponding provider/autoload package?",
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
	assert.Contains(t, err.Error(), "plugins.kafka has configuration but no corresponding plugin", "multiple orphan sections must be listed all at once, cannot return after reporting one")
	assert.Contains(t, err.Error(), "plugins.mq has configuration but no corresponding plugin")
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
	require.NoError(t, err, "gorm appears in Snapshot, even if this run is disabled with enabled:false, it should not be considered an orphan configuration section")
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
	require.Error(t, err, "misspelled plugin key not in Snapshot, must report orphan configuration error")
	assert.Contains(t, err.Error(), "plugins.gorn has configuration but no corresponding plugin")
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
	require.NoError(t, err, "xbc/log/app namespace must be exempt from orphan diagnosis")
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
		"each expand must call a new Factory, cannot reuse values across Containers")
	assert.NotSame(t, p1, p2, "the plugin values expanded by each Container must be different objects")
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
	assert.Len(t, seen, 2, "named instances must come from separate Factory calls, cannot share the same object")
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
	assert.Contains(t, err.Error(), `plugin nil-result's Factory returned nil for instance "default"`)
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
	assert.Contains(t, err.Error(), `plugin typed-nil-result's Factory returned nil for instance "default"`)
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
	assert.Contains(t, err.Error(), `plugin typed-nil-unsafe-pointer's Factory returned nil for instance "default"`)
}

func TestInstanceIDAndLabel(t *testing.T) {
	single := &Instance{key: "cors", instance: defaultInstance, plugin: &fakeSinglePlugin{}}
	assert.Equal(t, "cors", single.ID())
	assert.Equal(t, "cors", single.Label(), "default instance of single-instance plugin, Label and ID are the same, no [default] added")

	multiDefault := &Instance{key: "gorm", multiple: true, instance: defaultInstance, plugin: &fakeMultiPlugin{}}
	assert.Equal(t, "gorm", multiDefault.ID(), "default is omitted in ID()")
	assert.Equal(t, "gorm[default]", multiDefault.Label(), "Label() must explicitly mark the default of multi-instance plugin")

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
	require.NotNil(t, p.Ctx(), "BindRuntimeContext must bind Context into Base during expand phase, not leave it until Init")
	assert.Same(t, insts[0].ctx, p.Ctx(), "Base.Ctx() must be the same object as the *plugin.Context held by the Instance, not a newly constructed one")
	assert.Equal(t, "basewired", p.Name(), "Base.Name() must bind to Definition.Key, not instance name, nor plugin's own type name")

	rl, ok := p.Log().(*recordingLogger)
	require.True(t, ok, "Base.Log() must return the truly bound instance logger, not silently fall back to global log.L()")
	assert.Contains(t, rl.fields, "plugin", "instance logger must carry the plugin field, proving it is a child logger derived from newInstance, not coming from nowhere")
	assert.Contains(t, rl.fields, "basewired")
	assert.Equal(t, "visible", p.Ctx().Config().Get("own"),
		"Context.Config path must be relative to the current Definition's configuration section")
	assert.Nil(t, p.Ctx().Config().Get("app.secret"),
		"instance-scoped configuration cannot read application or other owner's global keys")
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
		require.NotNil(t, p.Ctx(), "each named instance must be bound by BindRuntimeContext, not just the first one")
		assert.Equal(t, "gorm", p.Name(), "Base.Name() returns the text of Definition key, not changing with instance name")
		byInstance[inst.instance] = p
	}
	require.Len(t, byInstance, 2)
	assert.NotSame(t, byInstance["default"].Ctx(), byInstance["readonly"].Ctx(),
		"each named instance of multi-instance plugin must get its own *plugin.Context, not share the same object")
	assert.Equal(t, "default", byInstance["default"].Ctx().Instance())
	assert.Equal(t, "readonly", byInstance["readonly"].Ctx().Instance())
	assert.Equal(t, "primary", byInstance["default"].Ctx().Config().Get("dsn"))
	assert.Equal(t, "replica", byInstance["readonly"].Ctx().Config().Get("dsn"),
		"multi-instance Context.Config must be limited to the current instance, not see sibling instances")
}
