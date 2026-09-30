package gracefulshutdown

import (
	"testing"

	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

// effectiveConfigPath reports the configuration section a Definition ends up
// owning, derived the way plugin/assembly derives it: an explicit ConfigPath
// wins, otherwise the conventional "plugins.<key>".
//
// The rule is restated here rather than imported because this package lives in
// the transport/web module, whose architecture guard forbids any of its
// packages -- production or test -- from depending on plugin/assembly. The
// duplication is the price of being able to assert the invariant at all from
// the package that owns the declarations.
func effectiveConfigPath(t *testing.T, definition plugin.Definition) string {
	t.Helper()
	descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(definition))
	if !ok {
		t.Fatal("DescribeDefinition returned no descriptor for a declared Definition")
	}
	if descriptor.ConfigPath != "" {
		return descriptor.ConfigPath
	}
	return "plugins." + descriptor.Key.String()
}

// TestBundleDefinitionsOwnDistinctConfigSections is the regression guard for a
// composition that could not start at all.
//
// Both Definitions used to resolve to plugins.gracefulshutdown: the Controller
// by convention from its key, the HTTP adapter from an explicit ConfigPath
// naming the same path. One path claimed by two owners is an error the
// configuration layer raises while building the universe, so selecting Bundle()
// failed before a single plugin was constructed -- and no test that merely read
// the bundle could see it.
func TestBundleDefinitionsOwnDistinctConfigSections(t *testing.T) {
	controller := effectiveConfigPath(t, Definition())
	http := effectiveConfigPath(t, httpDefinition)

	if controller != "plugins.gracefulshutdown" {
		t.Fatalf("Controller section = %q, want plugins.gracefulshutdown", controller)
	}
	if http != "plugins.gracefulshutdown-http" {
		t.Fatalf("HTTP adapter section = %q, want plugins.gracefulshutdown-http", http)
	}
	if controller == http {
		t.Fatalf("both Definitions claim section %q, which no composition can accept", controller)
	}
}

// TestDefinitionsWatchTheirOwnSections pins each Activation to the section its
// own Definition owns.
//
// An Activation left pointing at the other Definition's path would not fail:
// the plugin would simply stay disabled, or be enabled by a section that has
// nothing to say about it. That silence is the failure mode this file exists to
// prevent, so the paths are asserted rather than assumed to follow the keys.
func TestDefinitionsWatchTheirOwnSections(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		definition plugin.Definition
		section    string
	}{
		{"controller", Definition(), controllerConfigPath},
		{"http adapter", httpDefinition, httpConfigPath},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(testCase.definition))
			if !ok {
				t.Fatal("DescribeDefinition returned no descriptor for a declared Definition")
			}
			if descriptor.Activation.Kind != pluginmodel.ActivationConfigured {
				t.Fatalf("activation kind = %v, want ActivationConfigured", descriptor.Activation.Kind)
			}
			if descriptor.Activation.Path != testCase.section {
				t.Fatalf("activation path = %q, want %q", descriptor.Activation.Path, testCase.section)
			}
			if section := effectiveConfigPath(t, testCase.definition); section != testCase.section {
				t.Fatalf("section = %q, want the watched section %q", section, testCase.section)
			}
		})
	}
}
