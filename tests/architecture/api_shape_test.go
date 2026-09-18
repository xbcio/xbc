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
	"github.com/xbcio/xbc/plugin/assembly"
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

// TestArchAssemblyPlanPublicAPIShape locks the read-only surface a diagnostic
// reads a frozen graph through. Two rules are semantic rather than cosmetic: a
// plan answers questions in terms of identities, configuration paths, contracts
// and composition sites, and it never hands out an internal input token
// identifier, which is a per-process counter with no meaning outside wiring.
func TestArchAssemblyPlanPublicAPIShape(t *testing.T) {
	planType := reflect.TypeOf((*assembly.Plan)(nil))

	want := map[string]reflect.Type{
		"Order":              reflect.TypeOf((func(*assembly.Plan) []plugin.Identity)(nil)),
		"DefinitionCount":    reflect.TypeOf((func(*assembly.Plan) int)(nil)),
		"Disabled":           reflect.TypeOf((func(*assembly.Plan) []plugin.Key)(nil)),
		"DisabledDetail":     reflect.TypeOf((func(*assembly.Plan) []assembly.DisabledDefinition)(nil)),
		"InstanceConfigPath": reflect.TypeOf((func(*assembly.Plan, plugin.Identity) string)(nil)),
		"InstanceSelectedAt": reflect.TypeOf((func(*assembly.Plan, plugin.Identity) string)(nil)),
		"InstanceInputs":     reflect.TypeOf((func(*assembly.Plan, plugin.Identity) []assembly.InputEdge)(nil)),
		"Contracts":          reflect.TypeOf((func(*assembly.Plan, reflect.Type) []plugin.Identity)(nil)),
		// The workload accessors answer "what does this process carry, and
		// why", which is a different question from every accessor above: those
		// describe the graph that was built, while these also describe the
		// workloads deliberately left out of it. Workloads is the whole
		// declared set with the hosting decision, WorkloadOf attributes one
		// identity for resource accounting, and Placement names the source
		// that decided.
		"Workloads":  reflect.TypeOf((func(*assembly.Plan) []assembly.PlanWorkload)(nil)),
		"Placement":  reflect.TypeOf((func(*assembly.Plan) plugin.Placement)(nil)),
		"WorkloadOf": reflect.TypeOf((func(*assembly.Plan, plugin.Identity) (plugin.WorkloadKey, bool))(nil)),
	}
	for name, signature := range want {
		method, ok := planType.MethodByName(name)
		require.True(t, ok, "assembly.Plan.%s must exist", name)
		assert.Equal(t, signature, method.Type, "assembly.Plan.%s signature drift", name)
	}
	for index := 0; index < planType.NumMethod(); index++ {
		method := planType.Method(index)
		_, expected := want[method.Name]
		assert.Truef(t, expected, "assembly.Plan exposes unreviewed accessor %s", method.Name)
	}

	edgeFields := map[string]reflect.Type{
		"Contract":  reflect.TypeOf((*reflect.Type)(nil)).Elem(),
		"Query":     reflect.TypeOf(assembly.InputQuery("")),
		"Producers": reflect.TypeOf([]plugin.Identity(nil)),
	}
	edgeType := reflect.TypeOf(assembly.InputEdge{})
	for name, fieldType := range edgeFields {
		field, ok := edgeType.FieldByName(name)
		require.True(t, ok, "assembly.InputEdge.%s must exist", name)
		assert.Equal(t, fieldType, field.Type, "assembly.InputEdge.%s type drift", name)
	}
	assert.Equal(t, len(edgeFields), edgeType.NumField(),
		"assembly.InputEdge must not grow a field without review, in particular not an input token identifier")
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
// plugin.Context and runtime. The important rule is semantic: this
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
