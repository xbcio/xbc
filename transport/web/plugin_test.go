package web

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

func TestDefinitionIsStableAndValid(t *testing.T) {
	def := Definition()

	assert.Equal(t, Key, def.Key)
	assert.Equal(t, plugin.SingleInstance, def.Instances)
	assert.Equal(t, plugin.Always, def.Activation)
	require.NoError(t, def.Validate())
	assert.IsType(t, new(Server), def.Factory())
}
