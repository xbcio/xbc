package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/extensions/httpclient"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
	pluginautoload "github.com/xbcio/xbc/plugin/autoload"
)

func TestImportDeclaresHTTPClientBundle(t *testing.T) {
	environment, err := config.NewEnvironment(map[string]any{
		"plugins": map[string]any{"http-client": map[string]any{"gateway": map[string]any{}}},
	}, "XBC_HTTPCLIENT_AUTOLOAD_TEST_")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{pluginautoload.Freeze()},
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("BuildPlan(autoload.Freeze()) error = %v", err)
	}
	if plan.DefinitionCount() != 1 {
		t.Fatalf("autoload Definition count = %d, want 1", plan.DefinitionCount())
	}
	want := []plugin.Identity{{Plugin: httpclient.Key, Instance: "gateway"}}
	if got := plan.Order(); !reflect.DeepEqual(got, want) {
		t.Fatalf("autoload plan order = %#v, want %#v", got, want)
	}
}
