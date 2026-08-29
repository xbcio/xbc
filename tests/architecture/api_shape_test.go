package architecture_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/ordering"
)

// TestArchOrderingPublicAPIShape locks the small cross-module surface that
// defines ordering across modules. Sort behavior has focused tests in plugin/ordering;
// this guard only protects the public names and signatures other modules compile
// against, including the deliberate removal of AddEdge(..., hard bool).
func TestArchOrderingPublicAPIShape(t *testing.T) {
	graphType := reflect.TypeOf((*ordering.Graph)(nil))
	wantEdgeMethod := reflect.TypeOf((func(*ordering.Graph, string, string))(nil))
	for _, name := range []string{"AddHardEdge", "AddSoftEdge"} {
		method, ok := graphType.MethodByName(name)
		require.True(t, ok, "ordering.Graph.%s 必须存在", name)
		assert.Equal(t, wantEdgeMethod, method.Type, "ordering.Graph.%s 签名必须是 (string, string)", name)
	}
	_, hasLegacyAddEdge := graphType.MethodByName("AddEdge")
	assert.False(t, hasLegacyAddEdge, "含义模糊的 ordering.Graph.AddEdge(..., hard bool) 不得复活")

	var after ordering.Direction = ordering.After
	var before ordering.Direction = ordering.Before
	directionType := reflect.TypeOf(after)
	assert.Equal(t, "Direction", directionType.Name(), "After/Before 必须属于命名类型 ordering.Direction")
	assert.Equal(t, "github.com/xbcio/xbc/plugin/ordering", directionType.PkgPath())
	assert.NotEqual(t, after, before, "After 与 Before 必须是不同的 typed direction")
}

// TestArchRuntimeHostPublicAPIShape locks the deliberately narrow reverse
// port between plugin.Context and package runtime's hostAdapter. Test doubles would
// catch many signature changes at compile time, but reflection makes the
// architectural rule explicit and also catches a method being removed or a
// fifth convenience method being added.
func TestArchRuntimeHostPublicAPIShape(t *testing.T) {
	hostType := reflect.TypeOf((*plugin.RuntimeHost)(nil)).Elem()
	require.Equal(t, reflect.Interface, hostType.Kind())
	require.Equal(t, 4, hostType.NumMethod(), "plugin.RuntimeHost 必须保持四方法窄端口")

	want := map[string]reflect.Type{
		"ProvideValue":       reflect.TypeOf((func(reflect.Type, string, any))(nil)),
		"LookupValue":        reflect.TypeOf((func(reflect.Type, string) (any, error))(nil)),
		"InitializedPlugins": reflect.TypeOf((func() ([]plugin.Extension[any], error))(nil)),
		"GoManaged":          reflect.TypeOf((func(plugin.Identity, func(context.Context), bool))(nil)),
	}
	for name, signature := range want {
		method, ok := hostType.MethodByName(name)
		require.True(t, ok, "plugin.RuntimeHost.%s 必须存在", name)
		assert.Equal(t, signature, method.Type, "plugin.RuntimeHost.%s 签名漂移", name)
	}
}
