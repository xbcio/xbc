// Package autoload declares session's canonical Bundle in XBC's optional
// process-global composition. Import it only for side effects from an
// executable.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/web/integrations/session"
)

func init() { autoload.Declare(session.Bundle()) }
