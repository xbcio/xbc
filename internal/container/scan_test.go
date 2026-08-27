package container

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

type scalarMarker int
type sliceMarker []string

type skippedTagMarker struct {
	Ignored int `xbc:"-"`
}

type nilEmbeddedBaseMarker struct {
	*plugin.Base
}

func TestAssembleAcceptsNonNilMarkerValuesWithoutScannableFields(t *testing.T) {
	n := 7
	cases := []struct {
		name  string
		value plugin.Plugin
	}{
		{name: "plain struct value", value: struct{}{}},
		{name: "scalar value", value: scalarMarker(1)},
		{name: "slice value", value: sliceMarker{}},
		{name: "pointer to scalar", value: &n},
		{name: "struct value with explicit skip", value: skippedTagMarker{}},
		{name: "nil optional embedded Base", value: nilEmbeddedBaseMarker{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := plugin.Definition{
				Key:     "marker",
				Factory: func() plugin.Plugin { return tc.value },
			}
			c := newTestContainer(t, freezeDefs(t, def), nil)

			require.NotPanics(t, func() {
				require.NoError(t, c.Assemble())
			})
			require.Len(t, c.Order(), 1)
			assert.Equal(t, plugin.Key("marker"), c.Order()[0].Key())
			assert.Equal(t, reflect.TypeOf(tc.value), reflect.TypeOf(c.Order()[0].Plugin()))
		})
	}
}

type valueDeclarerMarker struct{}

func (valueDeclarerMarker) Dependencies() plugin.Deps {
	return plugin.Deps{Plugins: []plugin.Ref{plugin.RefTo("target")}}
}

func TestStructValueMarkerStillParticipatesInDependencyResolution(t *testing.T) {
	consumer := plugin.Definition{
		Key:     "consumer",
		Factory: func() plugin.Plugin { return valueDeclarerMarker{} },
	}
	target := plugin.Definition{
		Key:     "target",
		Factory: func() plugin.Plugin { return struct{}{} },
	}
	c := newTestContainer(t, freezeDefs(t, consumer, target), nil)

	require.NoError(t, c.Assemble())
	assert.Equal(t, []string{"target", "consumer"}, idsOf(c.Order()),
		"marker 的动态值形状不应妨碍其 Dependencies 回调参与建图")
}

type taggedStructValueMarker struct {
	Dependency *int `xbc:"inject,optional"`
}

func TestAssembleRejectsTaggedStructValueInsteadOfSilentlyIgnoringDeclaration(t *testing.T) {
	def := plugin.Definition{
		Key:     "tagged-value",
		Factory: func() plugin.Plugin { return taggedStructValueMarker{} },
	}
	c := newTestContainer(t, freezeDefs(t, def), nil)

	err := c.Assemble()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tagged-value")
	assert.Contains(t, err.Error(), "xbc tag")
	assert.Contains(t, err.Error(), "struct 指针")
}

type valueInitializerMarker struct {
	called *bool
}

func (m valueInitializerMarker) Init(*plugin.Context) error {
	*m.called = true
	return nil
}

func TestStructValueMarkerCanRunValueReceiverLifecycle(t *testing.T) {
	called := false
	def := plugin.Definition{
		Key:     "value-initializer",
		Factory: func() plugin.Plugin { return valueInitializerMarker{called: &called} },
	}
	c := newTestContainer(t, freezeDefs(t, def), nil)

	require.NoError(t, c.Assemble())
	require.NoError(t, runLifecycle(c, c.Order()))
	assert.True(t, called, "非指针 marker 的 value-receiver 生命周期接口必须正常执行")
}
