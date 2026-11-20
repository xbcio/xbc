// Package autoload declares the Casbin engine's canonical Bundle in XBC's
// optional process-global composition. Import it only for side effects from an
// executable.
package autoload

import (
	"github.com/xbcio/xbc/extensions/authorization/casbinx"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() { autoload.Declare(casbinx.Bundle()) }
