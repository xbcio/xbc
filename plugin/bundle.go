package plugin

import pluginmodel "github.com/xbcio/xbc/plugin/model"

// Bundle is a side-effect-free static collection of canonical Definitions. It
// has no runtime identity, configuration, dependencies, or lifecycle.
type Bundle pluginmodel.Bundle

// BundleOf creates a Bundle from canonical Definition handles.
func BundleOf(definitions ...Definition) Bundle {
	erased := make([]pluginmodel.Definition, len(definitions))
	for i, definition := range definitions {
		erased[i] = pluginmodel.Definition(definition)
	}
	return Bundle(pluginmodel.NewBundle(callerOrigin(1), erased...))
}

// CombineBundles returns a flattened static composition while preserving each
// declaration occurrence's original source.
func CombineBundles(bundles ...Bundle) Bundle {
	erased := make([]pluginmodel.Bundle, len(bundles))
	for i, bundle := range bundles {
		erased[i] = pluginmodel.Bundle(bundle)
	}
	return Bundle(pluginmodel.CombineBundles(erased...))
}
