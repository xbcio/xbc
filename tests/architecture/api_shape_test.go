package architecture_test

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
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
		require.True(t, ok, "ordering.Graph.%s must exist", name)
		assert.Equal(t, wantEdgeMethod, method.Type, "ordering.Graph.%s signature must be (string, string)", name)
	}
	_, hasLegacyAddEdge := graphType.MethodByName("AddEdge")
	assert.False(t, hasLegacyAddEdge, "Ambiguous ordering.Graph.AddEdge(..., hard bool) must not be revived")

	var after ordering.Direction = ordering.After
	var before ordering.Direction = ordering.Before
	directionType := reflect.TypeOf(after)
	assert.Equal(t, "Direction", directionType.Name(), "After/Before must belong to named type ordering.Direction")
	assert.Equal(t, "github.com/xbcio/xbc/plugin/ordering", directionType.PkgPath())
	assert.NotEqual(t, after, before, "After and Before must be different typed directions")
}

// TestArchRetiredCompositionAPIsStayRetired checks declarations, not call sites.
// The migration inventory rejects uses throughout the workspace; this guard also
// prevents an unused Registry/Provider/Extensions compatibility surface from
// being reintroduced in the public packages without yet having a caller.
func TestArchRetiredCompositionAPIsStayRetired(t *testing.T) {
	root := archRepositoryRoot(t)
	cases := []struct {
		name    string
		dir     string
		retired []string
	}{
		{
			name: "plugin",
			dir:  filepath.Join(root, "plugin"),
			retired: []string{
				"Always", "Base", "BindLifecycleContext", "Configurable", "Configured",
				"Declarer", "Dep", "Deps", "Extension", "Extensions", "Get", "GetNamed",
				"MustGet", "MustGetNamed", "Need", "NeedNamed", "Offer", "Opt", "Plugin",
				"Provide", "Provider", "Registry",
			},
		},
		{
			name:    "transport/web",
			dir:     filepath.Join(root, "transport", "web"),
			retired: []string{"MiddlewareProvider", "RouteCatalogConsumer", "RouteProvider"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exported := archExportedNamesInDir(t, tc.dir)
			for _, name := range tc.retired {
				assert.NotContainsf(t, exported, name, "%s must not restore retired composition API %s", tc.name, name)
			}
		})
	}
}

// TestArchRuntimeHostPublicAPIShape locks the capability boundary between
// plugin.Context and internal/runtime. The important rule is semantic: this
// reverse port must never regain value publication, lookup, or enumeration.
func TestArchRuntimeHostPublicAPIShape(t *testing.T) {
	hostType := reflect.TypeOf((*plugin.RuntimeHost)(nil)).Elem()
	require.Equal(t, reflect.Interface, hostType.Kind())

	want := map[string]reflect.Type{
		"ExecutionContext": reflect.TypeOf((func() context.Context)(nil)),
		"Logger":           reflect.TypeOf((func() log.Logger)(nil)),
		"TrafficGate":      reflect.TypeOf((func() <-chan struct{})(nil)),
		"SubmitTask":       reflect.TypeOf((func(plugin.Identity, func(context.Context), bool) bool)(nil)),
		"RequestShutdown":  reflect.TypeOf((func(plugin.Identity, string) bool)(nil)),
	}
	for name, signature := range want {
		method, ok := hostType.MethodByName(name)
		require.True(t, ok, "plugin.RuntimeHost.%s must exist", name)
		assert.Equal(t, signature, method.Type, "plugin.RuntimeHost.%s signature drift", name)
	}

	for index := 0; index < hostType.NumMethod(); index++ {
		method := hostType.Method(index)
		_, expected := want[method.Name]
		assert.Truef(t, expected, "plugin.RuntimeHost exposes unreviewed capability %s", method.Name)
	}
	for _, retired := range []string{
		"Provide", "ProvideValue", "Get", "Lookup", "LookupValue",
		"Resolve", "Extensions", "InitializedPlugins", "Enumerate", "Scan",
	} {
		_, exists := hostType.MethodByName(retired)
		assert.Falsef(t, exists, "plugin.RuntimeHost must not expose retired publication/lookup/enumeration method %s", retired)
	}
}
