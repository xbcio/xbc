package outbox_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xbcio/xbc/integrations/outbox"
	"github.com/xbcio/xbc/internal/assembly"
	"github.com/xbcio/xbc/internal/autoload"
	"github.com/xbcio/xbc/plugin"
)

func TestOrdinaryImportDefinitionAndBundleAreSideEffectFree(t *testing.T) {
	first := outbox.Definition()
	assert.True(t, first == outbox.Definition())
	_ = outbox.Bundle()

	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{autoload.Freeze()},
	})
	require.NoError(t, err)
	assert.Zero(t, plan.DefinitionCount(),
		"ordinary imports and Bundle calls must not mutate default composition; executables opt in through integrations/outbox/autoload")
}
