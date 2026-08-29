package assembly

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

type literalDeclarationsPlugin struct {
	deps     plugin.Deps
	provides []plugin.Dep
}

func (p *literalDeclarationsPlugin) Dependencies() plugin.Deps { return p.deps }
func (p *literalDeclarationsPlugin) Provides() []plugin.Dep    { return p.provides }

func TestResolveRejectsMalformedDynamicDeclarationsWithoutPanicking(t *testing.T) {
	productType := reflect.TypeOf((*svcA)(nil))
	cases := []struct {
		name     string
		deps     plugin.Deps
		provides []plugin.Dep
		wantPath string
		want     string
	}{
		{
			name:     "dependency nil type",
			deps:     plugin.Deps{Types: []plugin.Dep{{Type: nil}}},
			wantPath: "Dependencies()",
			want:     "Type cannot be empty",
		},
		{
			name:     "dependency invalid instance",
			deps:     plugin.Deps{Types: []plugin.Dep{{Type: productType, Instance: "read.only"}}},
			wantPath: "Types[0]",
			want:     "Instance \"read.only\" is invalid",
		},
		{
			name:     "zero ref",
			deps:     plugin.Deps{Plugins: []plugin.Ref{{}}},
			wantPath: "Plugins[0].Key",
			want:     "cannot be empty",
		},
		{
			name:     "ref invalid key",
			deps:     plugin.Deps{Plugins: []plugin.Ref{plugin.RefTo("Bad.Key")}},
			wantPath: "Plugins[0].Key",
			want:     "contains invalid character",
		},
		{
			name:     "ref invalid instance",
			deps:     plugin.Deps{Plugins: []plugin.Ref{plugin.RefTo("cache").Instance("read.only")}},
			wantPath: "Plugins[0].Instance",
			want:     "contains invalid character",
		},
		{
			name:     "after zero key",
			deps:     plugin.Deps{After: []plugin.Key{""}},
			wantPath: "After[0]",
			want:     "cannot be empty",
		},
		{
			name:     "before invalid key",
			deps:     plugin.Deps{Before: []plugin.Key{"Bad"}},
			wantPath: "Before[0]",
			want:     "contains invalid character",
		},
		{
			name:     "provide nil type",
			provides: []plugin.Dep{{Type: nil}},
			wantPath: "Provides()",
			want:     "Type cannot be empty",
		},
		{
			name:     "provide instance unsupported",
			provides: []plugin.Dep{{Type: productType, Instance: "readonly"}},
			wantPath: ".Instance",
			want:     "Instance is not supported",
		},
		{
			name:     "provide optional unsupported",
			provides: []plugin.Dep{{Type: productType, Optional: true}},
			wantPath: ".Optional",
			want:     "Optional is not supported",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Container{}
			inst := newInst(t, "malformed", "readonly", &literalDeclarationsPlugin{
				deps: tc.deps, provides: tc.provides,
			})
			_, _, err := c.resolve([]*Instance{inst})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "malformed")
			assert.Contains(t, err.Error(), "readonly")
			assert.Contains(t, err.Error(), tc.wantPath)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

type providesOncePlugin struct {
	Out   *svcA `xbc:"provide"`
	calls int
}

func (p *providesOncePlugin) Provides() []plugin.Dep {
	p.calls++
	if p.calls > 1 {
		panic("Provides called more than once")
	}
	return []plugin.Dep{plugin.Offer[*svcA]()}
}

func TestProvidesCalledOncePerInstanceAndHarvestUsesCache(t *testing.T) {
	var created []*providesOncePlugin
	def := plugin.Definition{
		Key: "once-provider",
		Factory: func() plugin.Plugin {
			instance := &providesOncePlugin{Out: &svcA{}}
			created = append(created, instance)
			return instance
		},
		Instances:  plugin.MultipleInstances,
		Activation: plugin.Always,
	}
	c := newTestContainer(t, freezeDefs(t, def), map[string]any{
		"plugins": map[string]any{
			"once-provider": map[string]any{
				"default":  map[string]any{},
				"readonly": map[string]any{},
			},
		},
	})

	require.NoError(t, c.Assemble())
	require.Len(t, created, 2)
	require.Len(t, c.Order(), 2)
	for _, instance := range created {
		assert.Equal(t, 1, instance.calls, "each instance must call Provides() exactly once during assembly")
	}
	for _, instance := range c.Order() {
		require.NoError(t, c.Harvest(instance))
	}
	for _, instance := range created {
		assert.Equal(t, 1, instance.calls, "Harvest must use Instance cache, cannot call Provides() again")
	}
}
