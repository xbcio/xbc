// Package autoload declares JWT's canonical Bundle in XBC's optional
// process-global composition. Import it only for side effects from an executable.
package autoload

import (
	"github.com/xbcio/xbc/extensions/authentication/jwtx"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() { autoload.Declare(jwtx.Bundle()) }
