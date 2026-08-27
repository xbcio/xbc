package container

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

// reusedImplementation deliberately backs two unrelated definition keys.
// Dependency identity must remain the key, never this concrete Go type.
type reusedImplementation struct{}

type keyRefConsumer struct{}

func (*keyRefConsumer) Dependencies() plugin.Deps {
	return plugin.Deps{Plugins: []plugin.Ref{plugin.RefTo("target")}}
}

func TestResolveRefRejectsSameImplementationUnderWrongKey(t *testing.T) {
	c := &Container{}
	consumer := newInst(t, "consumer", defaultInstance, &keyRefConsumer{})
	decoy := newInst(t, "decoy", defaultInstance, &reusedImplementation{})

	_, _, err := c.resolve([]*Instance{consumer, decoy})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "依赖插件 target")
	assert.Contains(t, err.Error(), "target 未启用")
}

func TestResolveRefAcceptsKeyRegardlessOfImplementationType(t *testing.T) {
	c := &Container{}
	consumer := newInst(t, "consumer", defaultInstance, &keyRefConsumer{})
	target := newInst(t, "target", defaultInstance, &reusedImplementation{})
	decoy := newInst(t, "decoy", defaultInstance, &reusedImplementation{})

	order, misses, err := c.resolve([]*Instance{consumer, target, decoy})
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t, []string{"target", "consumer", "decoy"}, idsOf(order))
}
