package autoload

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
	defaultautoload "github.com/xbcio/xbc/plugin/autoload"
	casbingorm "github.com/xbcio/xbc/transport/web/integrations/casbin-gorm"
)

func TestImportDeclaresCasbinGORMBundle(t *testing.T) {
	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{defaultautoload.Freeze()},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, plan.DefinitionCount())
	assert.Equal(t, []plugin.Key{casbingorm.Key}, plan.Disabled())
}
