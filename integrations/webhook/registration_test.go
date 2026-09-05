package webhook_test

import (
	"testing"

	"github.com/xbcio/xbc/integrations/webhook"
	"github.com/xbcio/xbc/internal/assembly"
	"github.com/xbcio/xbc/internal/autoload"
	"github.com/xbcio/xbc/plugin"
)

func TestOrdinaryPackageImportHasNoCompositionSideEffect(t *testing.T) {
	_ = webhook.Key
	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{autoload.Freeze()},
	})
	if err != nil {
		t.Fatalf("BuildPlan(autoload.Freeze()) error = %v", err)
	}
	if plan.DefinitionCount() != 0 {
		t.Fatalf("ordinary webhook import declared %d Definitions", plan.DefinitionCount())
	}
}
