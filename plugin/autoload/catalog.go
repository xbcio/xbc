// Package autoload is the optional process-global adapter used only by leaf
// autoload packages and xbc.Run. Explicit xbc.WithBundles composition does not
// read or mutate this catalog.
package autoload

import (
	"fmt"
	"sync"

	"github.com/xbcio/xbc/plugin"
)

var defaultCatalog struct {
	sync.Mutex
	bundles  []plugin.Bundle
	frozen   bool
	snapshot plugin.Bundle
}

// Declare adds canonical Bundles to the optional process-global composition.
// Autoload packages call Declare from init; implementation and Prelude
// packages must remain side-effect free.
func Declare(bundles ...plugin.Bundle) {
	defaultCatalog.Lock()
	defer defaultCatalog.Unlock()
	if defaultCatalog.frozen {
		panic(fmt.Sprintf("xbc: autoload declaration after default composition was frozen (%d bundles)", len(bundles)))
	}
	defaultCatalog.bundles = append(defaultCatalog.bundles, bundles...)
}

// Freeze returns the immutable default composition. The first call closes
// declaration admission; subsequent calls return the same Bundle handle data.
func Freeze() plugin.Bundle {
	defaultCatalog.Lock()
	defer defaultCatalog.Unlock()
	if !defaultCatalog.frozen {
		defaultCatalog.snapshot = plugin.CombineBundles(defaultCatalog.bundles...)
		defaultCatalog.bundles = nil
		defaultCatalog.frozen = true
	}
	return defaultCatalog.snapshot
}
