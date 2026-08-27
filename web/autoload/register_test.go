package autoload

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/catalog"
	"github.com/xbcio/xbc/web"
)

func TestImportRegistersWebDefinition(t *testing.T) {
	snapshot, err := catalog.Freeze()
	require.NoError(t, err)

	def, ok := snapshot.Lookup(web.Key)
	require.True(t, ok)
	assert.Equal(t, web.Key, def.Key)
	assert.Equal(t, plugin.SingleInstance, def.Instances)
	assert.Equal(t, plugin.Always, def.Activation)
}
