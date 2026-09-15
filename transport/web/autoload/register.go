// Package autoload declares the curated Web prelude Bundle in XBC's optional
// process-wide composition. Import it only for side effects from an
// executable.
//
// Declaring prelude.Bundle() here is not, by itself, enough to assemble a
// startable app: web.Bundle()'s server Definition requires exactly one
// web.EngineFactory input, and prelude contributes none. autoload cannot make
// that choice either, for the same reason prelude cannot -- it lives inside
// the transport/web module, and importing an engine adapter would pull that
// engine's dependencies back into transport/web's dependency closure. The
// executable's composition root must still separately import and declare (or
// select via xbc.WithBundles) exactly one engine Bundle, such as
// transport/web/engines/gin.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/web/prelude"
)

func init() {
	autoload.Declare(prelude.Bundle())
}
