// Package autoload declares kafkax.Bundle into XBC's optional process-wide
// composition. Prefer explicit Bundle composition in applications.
package autoload

import (
	"github.com/xbcio/xbc/extensions/messaging/kafkax"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() { autoload.Declare(kafkax.Bundle()) }
