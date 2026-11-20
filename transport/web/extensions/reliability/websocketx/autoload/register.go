// Package autoload declares websocket's canonical Bundle in XBC's optional
// process-global composition. Import it only for side effects from an
// executable.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/web/extensions/reliability/websocketx"
)

func init() { autoload.Declare(websocketx.Bundle()) }
