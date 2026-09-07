// Package autoload declares the timeout Bundle in XBC's optional
// process-wide composition. Import it only for side effects from an executable.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/web/extensions/reliability/timeout"
)

func init() {
	autoload.Declare(timeout.Bundle())
}
