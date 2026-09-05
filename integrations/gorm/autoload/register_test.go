package autoload

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gormplugin "github.com/xbcio/xbc/integrations/gorm"
	"github.com/xbcio/xbc/internal/assembly"
	defaultautoload "github.com/xbcio/xbc/internal/autoload"
	"github.com/xbcio/xbc/plugin"
)

func TestImportDeclaresGormBundle(t *testing.T) {
	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{defaultautoload.Freeze()},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, plan.DefinitionCount())
	assert.Equal(t, []plugin.Key{gormplugin.Key}, plan.Disabled())
}
