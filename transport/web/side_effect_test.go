package web_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin/catalog"
	"github.com/xbcio/xbc/transport/web"
)

func TestOrdinaryImportAndDefinitionAreSideEffectFree(t *testing.T) {
	// Calling Definition exercises the ordinary package's public assembly API;
	// neither importing web nor obtaining its Definition may mutate the default
	// catalog. Executables opt in explicitly through transport/web/autoload.
	def := web.Definition()
	assert.Equal(t, web.Key, def.Key)

	snapshot, err := catalog.Freeze()
	require.NoError(t, err)
	assert.Zero(t, snapshot.Len(),
		"importing web must be side-effect free; executables opt in through transport/web/autoload")
}
