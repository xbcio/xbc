package gorm_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gormplugin "github.com/xbcio/xbc/integrations/gorm"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
	"github.com/xbcio/xbc/plugin/autoload"
)

func TestOrdinaryImportDefinitionAndBundleAreSideEffectFree(t *testing.T) {
	first := gormplugin.Definition()
	assert.True(t, first == gormplugin.Definition())
	_ = gormplugin.Bundle()

	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{autoload.Freeze()},
	})
	require.NoError(t, err)
	assert.Zero(t, plan.DefinitionCount(),
		"ordinary imports and Bundle calls must not mutate default composition; executables opt in through integrations/gorm/autoload")
}
