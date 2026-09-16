package assembly

import (
	"fmt"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

// pluginsRoot is the conventional configuration root the assembly package owns.
const pluginsRoot = "plugins"

// ConfigSections describes the configuration each selected Definition owns, so
// that the configuration layer can resolve environment variables against real
// schemas and reject a top-level key nobody claims. It runs before the
// configuration tree exists, which is exactly why it takes only Bundles.
//
// It freezes the same Bundles BuildPlan will freeze again; freezing is pure and
// deterministic, and keeping it here avoids leaking a half-built catalog into
// the caller.
func ConfigSections(bundles []plugin.Bundle) ([]config.Section, error) {
	selections, err := freezeBundles(bundles)
	if err != nil {
		return nil, err
	}
	sections := []config.Section{{
		Path:  pluginsRoot,
		Owner: "the assembly layer",
		Kind:  config.SectionNamespace,
	}}
	for _, selection := range selections {
		definition := selection.descriptor
		kind := config.SectionTyped
		if definition.Cardinality == pluginmodel.MultipleInstances {
			kind = config.SectionInstanced
		}
		section := config.Section{
			Path:   definitionPath(definition),
			Owner:  fmt.Sprintf("plugin %q", definition.Key),
			Kind:   kind,
			Toggle: true,
		}
		if definition.Config != nil {
			section.Schema = definition.Config.Type
		}
		sections = append(sections, section)
	}
	return sections, nil
}
