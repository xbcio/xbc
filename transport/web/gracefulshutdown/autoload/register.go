// Package autoload declares the gracefulshutdown Bundle in XBC's optional
// process-wide composition. Import it only for side effects from an executable.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/web/gracefulshutdown"
)

func init() {
	autoload.Declare(gracefulshutdown.Bundle())
}
