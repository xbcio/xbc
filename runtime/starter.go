package runtime

import (
	"fmt"

	"github.com/xbcio/xbc/plugin"
)

// Starter is plugin.Starter, re-exported so an application names the value it
// passes to WithStarter through the runtime it already imports.
//
// The interface itself lives in plugin rather than here because the starter a
// repository ships is a package of a transport module, and a transport module
// may not import runtime.
type Starter = plugin.Starter

// WithStarter selects a product-level baseline: the Bundles that make up a
// documented default composition, and the configuration layer that turns its
// dormant capabilities on without the application shipping a file.
//
// Selecting a starter is an explicit composition, exactly as WithBundles is,
// so the process-global autoload collector is not frozen alongside it. The
// starter's Bundles are composed first and the caller's own WithBundles after
// them; the order decides nothing, because planning collapses repeated
// canonical Definition handles and sorts the rest by their declared ordering.
//
// Defaults from the starter are the lowest layer of the merged configuration.
// Nothing in the framework reads them before a plugin does: they are handed to
// the loader, which is why a starter can turn a capability on that would
// otherwise stay dormant, and why the origin doctor prints for such a section
// names the starter instead of a file.
func WithStarter(starter Starter) Option {
	return func(options *appOptions) error {
		// A nil starter is rejected rather than read as "no baseline": the
		// caller wrote WithStarter because it meant to select one, and a
		// process that started with a silently different composition than its
		// root declares is the outcome this option exists to rule out.
		if starter == nil {
			return fmt.Errorf("xbc: WithStarter requires a non-nil Starter; omit the option to compose Bundles explicitly")
		}
		options.starter = starter
		options.hasStarter = true
		return nil
	}
}
