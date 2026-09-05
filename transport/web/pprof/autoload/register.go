// Package autoload declares the pprof Bundle in XBC's optional
// process-wide composition. Import it only for side effects from an executable.
package autoload

import (
	"github.com/xbcio/xbc/internal/autoload"
	"github.com/xbcio/xbc/transport/web/pprof"
)

func init() {
	autoload.Declare(pprof.Bundle())
}
