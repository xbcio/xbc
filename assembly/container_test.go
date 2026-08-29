package assembly

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/catalog"
)

// This file holds the mandatory new Container-level tests the task calls
// for: independence between two Containers built from the same Snapshot,
// InitializedPlugins' seal/order/defensive-copy contract, topological-order
// stability across repeated assembly and independent of a Snapshot's
// Declare order, and an Assemble-driven Activation.Configured(path) enable
// matrix (expand_test.go's TestExpandEnableRuleMatrix already covers the
// same ground at the expand()-only level; this one exercises the full
// Options -> Assemble -> Order/Disabled surface instead).

// orderProducer/orderConsumer form a two-node dependency chain (via
// Dependencies(), not a tag) purely to give
// TestTopologicalOrderStableAcrossRepeatedAssembly and
// TestInitializedPluginsErrorsBeforeSealAndReturnsOrderedCopyAfter something
// non-trivial to sort.
type orderProducer struct{}

type orderConsumer struct{}

func (p *orderConsumer) Dependencies() plugin.Deps {
	return plugin.Deps{Plugins: []plugin.Ref{plugin.RefTo("b")}}
}

func TestTopologicalOrderStableAcrossRepeatedAssembly(t *testing.T) {
	defA := plugin.Definition{Key: "a", Factory: func() plugin.Plugin { return &orderConsumer{} }, Activation: plugin.Always}
	defB := plugin.Definition{Key: "b", Factory: func() plugin.Plugin { return &orderProducer{} }, Activation: plugin.Always}

	// snap1 and snap2 declare the very same two Definitions in opposite
	// order -- catalog.Freeze sorts by Key before anything else runs, so
	// this must never leak into the topological order Assemble produces.
	snap1 := freezeDefs(t, defA, defB)
	snap2 := freezeDefs(t, defB, defA)

	var orders [][]string
	for _, snap := range []catalog.Snapshot{snap1, snap2, snap1, snap2} {
		c := newTestContainer(t, snap, nil)
		require.NoError(t, c.Assemble())
		orders = append(orders, idsOf(c.Order()))
	}
	for i := 1; i < len(orders); i++ {
		assert.Equal(t, orders[0], orders[i],
			"the topological order of the same dependency graph must remain stable across multiple independent assemblies and be independent of Declare order")
	}
	assert.Equal(t, []string{"b", "a"}, orders[0], "b is a hard dependency of a and must come before a")
}

// fakeIndependentValue is what independenceProducer's Init produces, tagged
// with which Container's Init call actually built it.
type fakeIndependentValue struct{ owner string }

type independenceProducer struct {
	Val *fakeIndependentValue `xbc:"provide"`
}

func (p *independenceProducer) Init(ctx *plugin.Context) error {
	p.Val = &fakeIndependentValue{owner: ctx.Instance()}
	return nil
}

// TestNewContainersFromSameSnapshotAreIndependent is the mandatory test
// registry.go's doc comment on valueRegistry refers to by name: two
// Containers built from the same immutable Snapshot must never share a
// plugin instance, a Context, or a registry entry.
func TestNewContainersFromSameSnapshotAreIndependent(t *testing.T) {
	def := plugin.Definition{
		Key:        "producer",
		Factory:    func() plugin.Plugin { return &independenceProducer{} },
		Activation: plugin.Always,
	}
	snap := freezeDefs(t, def)

	c1 := newTestContainer(t, snap, nil)
	require.NoError(t, c1.Assemble())
	require.Len(t, c1.Order(), 1)
	require.NoError(t, runLifecycle(c1, c1.Order()))

	c2 := newTestContainer(t, snap, nil)
	require.NoError(t, c2.Assemble())
	require.Len(t, c2.Order(), 1)
	require.NoError(t, runLifecycle(c2, c2.Order()))

	p1 := c1.Order()[0].Plugin().(*independenceProducer)
	p2 := c2.Order()[0].Plugin().(*independenceProducer)
	assert.NotSame(t, p1, p2, "the plugin instances expanded by each Container must be different objects")
	assert.NotSame(t, p1.Val, p2.Val, "the values produced by each Container's Init must also be different objects")
	assert.NotSame(t, c1.Order()[0].Context(), c2.Order()[0].Context(),
		"the Context of same-named instances in two Containers must be different objects")

	// Registry independence: a value provided into c1 must never leak into c2.
	type marker struct{}
	c1.ProvideValue(reflect.TypeOf(&marker{}), "default", &marker{})
	_, err := c2.LookupValue(reflect.TypeOf(&marker{}), "default")
	require.Error(t, err, "the value registered by c1 must never be found in c2's registry")
}

// TestInitializedPluginsErrorsBeforeSealAndReturnsOrderedCopyAfter pins down
// three separate, independently-checkable pieces of InitializedPlugins'
// contract in one assembled scenario: it errors before SealInitialization,
// it returns entries in the same topological order MarkInitialized was
// called in, and every call returns a fresh slice a caller cannot corrupt
// for the next caller.
func TestInitializedPluginsErrorsBeforeSealAndReturnsOrderedCopyAfter(t *testing.T) {
	defA := plugin.Definition{Key: "a", Factory: func() plugin.Plugin { return &orderConsumer{} }, Activation: plugin.Always}
	defB := plugin.Definition{Key: "b", Factory: func() plugin.Plugin { return &orderProducer{} }, Activation: plugin.Always}
	snap := freezeDefs(t, defA, defB)
	c := newTestContainer(t, snap, nil)
	require.NoError(t, c.Assemble())

	_, err := c.InitializedPlugins()
	require.Error(t, err, "queries must return an error before SealInitialization, cannot return a seemingly complete partial snapshot")
	assert.Contains(t, err.Error(), "not all plugins initialized yet")

	for _, inst := range c.Order() {
		require.NoError(t, runLifecycle(c, []*Instance{inst}))
		c.MarkInitialized(inst)
	}
	c.SealInitialization()

	exts, err := c.InitializedPlugins()
	require.NoError(t, err)
	require.Len(t, exts, 2)
	// b is a's hard dependency, so Order() (and therefore the
	// MarkInitialized call sequence this loop followed) puts b first.
	assert.Equal(t, plugin.Key("b"), exts[0].Identity.Plugin)
	assert.Equal(t, plugin.Key("a"), exts[1].Identity.Plugin)

	// Defensive copy: mutating a previously returned slice must never leak
	// into a later call's result.
	exts[0].Identity.Plugin = "corrupted"
	again, err := c.InitializedPlugins()
	require.NoError(t, err)
	assert.Equal(t, plugin.Key("b"), again[0].Identity.Plugin,
		"InitializedPlugins must return a new slice each time, preventing modifications to previous results from affecting future calls")
}

// TestAssembleActivationConfiguredEnableMatrix exercises the full
// Options -> Assemble -> Order()/Disabled() surface for the same Always/
// Configured(path) matrix expand_test.go's TestExpandEnableRuleMatrix
// already covers at the lower expand()-only level.
func TestAssembleActivationConfiguredEnableMatrix(t *testing.T) {
	cases := []struct {
		name        string
		activation  plugin.Activation
		data        map[string]any
		wantEnabled bool
	}{
		{
			"Configured path exists → enabled",
			plugin.Configured("plugins.cache"),
			map[string]any{"plugins": map[string]any{"cache": map[string]any{}}},
			true,
		},
		{
			"Configured path missing → disabled",
			plugin.Configured("plugins.cache"), nil, false,
		},
		{
			"Configured path exists but enabled:false → disabled",
			plugin.Configured("plugins.cache"),
			map[string]any{"plugins": map[string]any{"cache": map[string]any{"enabled": false}}},
			false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := plugin.Definition{
				Key:        "cache",
				Factory:    func() plugin.Plugin { return &fakeSinglePlugin{} },
				Activation: tc.activation,
			}
			c := newTestContainer(t, freezeDefs(t, def), tc.data)
			require.NoError(t, c.Assemble())

			if tc.wantEnabled {
				assert.Len(t, c.Order(), 1)
				assert.Empty(t, c.Disabled())
			} else {
				assert.Empty(t, c.Order())
				assert.Equal(t, []string{"cache"}, c.Disabled())
			}
		})
	}
}
