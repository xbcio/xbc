// Package autoload declares the cors Bundle in XBC's optional
// process-wide composition. Import it only for side effects from an executable.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/web/cors"
)

func init() {
	autoload.Declare(cors.Bundle())
}
