package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/integrations/webhook"
	"github.com/xbcio/xbc/internal/assembly"
	internalautoload "github.com/xbcio/xbc/internal/autoload"
	"github.com/xbcio/xbc/plugin"
)

func TestImportDeclaresWebhookBundle(t *testing.T) {
	environment, err := config.NewEnvironment(map[string]any{
		"plugins": map[string]any{"webhook": map[string]any{"partners": map[string]any{}}},
	}, "XBC_WEBHOOK_AUTOLOAD_TEST_")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{internalautoload.Freeze()},
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("BuildPlan(autoload.Freeze()) error = %v", err)
	}
	if plan.DefinitionCount() != 1 {
		t.Fatalf("autoload Definition count = %d, want 1", plan.DefinitionCount())
	}
	want := []plugin.Identity{{Plugin: webhook.Key, Instance: "partners"}}
	if got := plan.Order(); !reflect.DeepEqual(got, want) {
		t.Fatalf("autoload plan order = %#v, want %#v", got, want)
	}
}
