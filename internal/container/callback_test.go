package container

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

type panickingDependenciesPlugin struct{}

func (*panickingDependenciesPlugin) Dependencies() plugin.Deps {
	panic("dependencies exploded")
}

type panickingProvidesPlugin struct{}

func (*panickingProvidesPlugin) Provides() []plugin.Dep {
	panic("provides exploded")
}

type panickingConfigPlugin struct{}

func (*panickingConfigPlugin) ConfigPtr() any {
	panic("config exploded")
}

type configResultPlugin struct {
	target any
}

func (p *configResultPlugin) ConfigPtr() any { return p.target }

func assertCallbackPanic(t *testing.T, err error, key, instance, callback, panicValue string) {
	t.Helper()
	require.Error(t, err)
	assert.Contains(t, err.Error(), key)
	assert.Contains(t, err.Error(), instance)
	assert.Contains(t, err.Error(), callback)
	assert.Contains(t, err.Error(), panicValue)
	assert.Contains(t, err.Error(), "panic")
}

func TestAssemblyCallbacksRecoverPanicsWithIdentity(t *testing.T) {
	t.Run("Factory", func(t *testing.T) {
		def := plugin.Definition{
			Key: "factory-panic",
			Factory: func() plugin.Plugin {
				panic("factory exploded")
			},
			Activation: plugin.Always,
		}
		c := newTestContainer(t, freezeDefs(t, def), nil)
		err := c.Assemble()
		assertCallbackPanic(t, err, "factory-panic", "default", "Factory()", "factory exploded")
	})

	t.Run("Dependencies", func(t *testing.T) {
		c := &Container{}
		inst := newInst(t, "dependencies-panic", "readonly", &panickingDependenciesPlugin{})
		_, _, err := c.resolve([]*Instance{inst})
		assertCallbackPanic(t, err, "dependencies-panic", "readonly", "Dependencies()", "dependencies exploded")
	})

	t.Run("Provides", func(t *testing.T) {
		c := &Container{}
		inst := newInst(t, "provides-panic", "readonly", &panickingProvidesPlugin{})
		_, _, err := c.resolve([]*Instance{inst})
		assertCallbackPanic(t, err, "provides-panic", "readonly", "Provides()", "provides exploded")
	})

	t.Run("ConfigPtr", func(t *testing.T) {
		def := plugin.Definition{
			Key:        "config-panic",
			Factory:    func() plugin.Plugin { return &panickingConfigPlugin{} },
			Activation: plugin.Always,
		}
		c := newTestContainer(t, freezeDefs(t, def), nil)
		err := c.Assemble()
		assertCallbackPanic(t, err, "config-panic", "default", "ConfigPtr()", "config exploded")
	})
}

func TestConfigPtrRejectsInvalidReturnShapesBeforeBinding(t *testing.T) {
	var typedNil *struct{}
	cases := []struct {
		name   string
		target any
		want   string
	}{
		{name: "nil", target: nil, want: "<nil>"},
		{name: "typed nil", target: typedNil, want: "(nil)"},
		{name: "struct value", target: struct{}{}, want: "struct {}"},
		{name: "pointer to scalar", target: new(int), want: "*int"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := plugin.Definition{
				Key:        "bad-config",
				Factory:    func() plugin.Plugin { return &configResultPlugin{target: tc.target} },
				Activation: plugin.Always,
			}
			c := newTestContainer(t, freezeDefs(t, def), nil)
			err := c.Assemble()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "bad-config")
			assert.Contains(t, err.Error(), "default")
			assert.Contains(t, err.Error(), "ConfigPtr()")
			assert.Contains(t, err.Error(), "非 nil 的 struct 指针")
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
