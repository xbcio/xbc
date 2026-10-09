// Package autoload opts placement's Bundle into XBC's process-wide default
// composition. Libraries should import the side-effect-free parent package.
//
// It is half of an opt-in, never the whole of one: the Bundle keeps a Placement
// alive, and a process that blank-imports this package without also building
// one with placement.New has nothing for it to keep alive. That combination
// fails loudly at plan time rather than starting a process that renews and
// releases nothing, so the import belongs beside a composition root that
// already calls New and passes the result to WithPlacement.
package autoload

import (
	"github.com/xbcio/xbc/extensions/coordination/placement"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() { autoload.Declare(placement.Bundle()) }
