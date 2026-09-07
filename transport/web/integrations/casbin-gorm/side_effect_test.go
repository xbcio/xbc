package casbingorm_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
	"github.com/xbcio/xbc/plugin/autoload"
	casbingorm "github.com/xbcio/xbc/transport/web/integrations/casbin-gorm"
)

func TestOrdinaryImportDefinitionAndBundleAreSideEffectFree(t *testing.T) {
	first := casbingorm.Definition()
	assert.True(t, first == casbingorm.Definition())
	_ = casbingorm.Bundle()

	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{autoload.Freeze()},
	})
	require.NoError(t, err)
	assert.Zero(t, plan.DefinitionCount(),
		"ordinary imports and Bundle calls must not mutate default composition; executables opt in through casbin-gorm/autoload")
}
