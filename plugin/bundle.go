package plugin

import pluginmodel "github.com/xbcio/xbc/plugin/model"

// Bundle is a side-effect-free static collection of canonical Definitions. It
// has no runtime identity, configuration, dependencies, or lifecycle.
//
// Its occurrences are not anonymous: each may name the workload it belongs to,
// and the Bundle may carry the declarations of the workloads its occurrences
// name. Workload membership is composition data like everything else here, so
// reading it creates nothing and decides nothing -- assembly turns it into a
// hosted set before any factory runs.
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
// declaration occurrence's original source and its workload membership.
//
// A workload Bundle flattens exactly like any other Bundle here: combining is
// concatenation, so a composition root selects one the same way it selects an
// ordinary Bundle, and the workload declarations travel with the occurrences
// they describe.
func CombineBundles(bundles ...Bundle) Bundle {
	erased := make([]pluginmodel.Bundle, len(bundles))
	for i, bundle := range bundles {
		erased[i] = pluginmodel.Bundle(bundle)
	}
	return Bundle(pluginmodel.CombineBundles(erased...))
}
