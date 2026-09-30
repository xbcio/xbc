package main

import (
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
	"github.com/xbcio/xbc/transport/web"
	ginengine "github.com/xbcio/xbc/transport/web/engines/gin"
)

// assemblyCompanions lists the Bundles a Bundle's own graph requires but cannot
// supply itself, keyed by the accessor that needs them.
//
// Only web.Bundle() has one. web.Server's Definition carries a
// RequireOne[web.EngineFactory] input, and engine adapters are deliberately
// outside bundleProviders because each implements the neutral web.Engine port
// rather than a Web capability. A composition root that selects web.Bundle()
// must also select exactly one engine Bundle -- examples/quickstart does -- so
// the guard selects one too, instead of reporting a documented requirement of
// the composition model as a defect in the Bundle.
//
// Keep this list empty where a Bundle merely fails on an absent configuration
// section: that is a real defect, and the guard is what catches it.
var assemblyCompanions = []struct {
	provider   func() plugin.Bundle
	companions []func() plugin.Bundle
}{
	{web.Bundle, []func() plugin.Bundle{ginengine.Bundle}},
}

// companionsFor returns the Bundles that must be selected alongside the one
// provide declares, matched by function identity so a rename cannot silently
// detach a companion from the Bundle it belongs to.
func companionsFor(provide func() plugin.Bundle) []plugin.Bundle {
	target := reflect.ValueOf(provide).Pointer()
	for _, entry := range assemblyCompanions {
		if reflect.ValueOf(entry.provider).Pointer() != target {
			continue
		}
		bundles := make([]plugin.Bundle, 0, len(entry.companions))
		for _, companion := range entry.companions {
			bundles = append(bundles, companion())
		}
		return bundles
	}
	return nil
}

// TestCompositionBundlesAssemble assembles every Bundle this repository
// publishes, one at a time, under a default (empty) configuration.
//
// It exists because nothing else in the workspace ever assembles a real Bundle
// into a plan. tests/architecture/plugin_bundle_coverage_test.go only checks,
// at the AST level, that a Definition is reachable from its Bundle accessor,
// and this module's own snapshot tests only freeze the identity metadata of
// each Definition. Neither can observe a Bundle that freezes cleanly and then
// fails to plan -- which is exactly the shape of a duplicate configuration
// section within one Bundle: every Definition is individually valid, the
// conflict only exists between two of them.
//
// Membership is bundleProviders, the module's curated list of every reusable
// plugin package. This module is the only one in the workspace that depends on
// all of them at once: tests/ belongs to the root module, which does not
// require transport/web, so a cross-module assembly guard cannot live there.
//
// The guard deliberately runs the three stages the real bootstrap runs, in the
// same order (see runtime.bootstrap): ConfigSections declares one configuration
// section per selected Definition, config.NewUniverse rejects two Definitions
// claiming one path, and assembly.BuildPlan freezes, expands and wires the
// graph. A Bundle needs no placement source and no configuration file to reach
// the end of that sequence, so an empty PlanOptions is the honest default.
func TestCompositionBundlesAssemble(t *testing.T) {
	if len(bundleProviders) == 0 {
		t.Fatal("bundleProviders is empty, so this guard would pass vacuously")
	}
	for _, provide := range bundleProviders {
		t.Run(bundleProviderName(provide), func(t *testing.T) {
			bundle := provide()
			if bundleIsZero(bundle) {
				t.Fatal("Bundle() returned a zero Bundle, which nothing can plan")
			}
			// A Bundle that needs a companion is assembled the way a composition
			// root would select it, so the guard proves the pair plans rather
			// than proving the Bundle is incomplete.
			selected := append([]plugin.Bundle{bundle}, companionsFor(provide)...)

			// Stage one and two: the configuration ownership the composition
			// claims. This is where a duplicate section is caught, and it is
			// caught here rather than in BuildPlan because NewUniverse is the
			// first component that sees every section at once.
			sections, err := assembly.ConfigSections(selected)
			if err != nil {
				t.Fatalf("ConfigSections: %v", err)
			}
			if _, err := config.NewUniverse(sections...); err != nil {
				t.Fatalf("NewUniverse: %v", err)
			}

			// Stage three: the graph itself, from the same selection.
			if _, err := assembly.BuildPlan(assembly.PlanOptions{
				Bundles: selected,
			}); err != nil {
				t.Fatalf("BuildPlan: %v", err)
			}
		})
	}
}

// bundleIsZero reports whether a Bundle accessor returned the zero value, which
// assembly would reject only much later with a message about a zero Definition.
func bundleIsZero(bundle plugin.Bundle) bool {
	return reflect.ValueOf(bundle).IsZero()
}

// bundleProviderName names one Bundle accessor by its import path relative to
// this module's root, so a failing subtest points at the package to open rather
// than at a bare "Bundle". The last path segment alone is not unique: the core
// and Web reliability packages are both named "health".
func bundleProviderName(provide func() plugin.Bundle) string {
	const modulePrefix = "github.com/xbcio/xbc/"
	fn := runtime.FuncForPC(reflect.ValueOf(provide).Pointer())
	if fn == nil {
		return "unknown-bundle-provider"
	}
	return strings.TrimPrefix(fn.Name(), modulePrefix)
}
