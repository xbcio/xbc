package autoload

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xbcio/xbc/integrations/outbox"
	"github.com/xbcio/xbc/internal/assembly"
	defaultautoload "github.com/xbcio/xbc/internal/autoload"
	"github.com/xbcio/xbc/plugin"
)

func TestImportDeclaresOutboxBundle(t *testing.T) {
	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{defaultautoload.Freeze()},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, plan.DefinitionCount())
	assert.Equal(t, []plugin.Key{outbox.Key}, plan.Disabled())
}
