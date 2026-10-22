// Package autoload declares automemlimit's canonical Bundle in XBC's optional
// process-wide composition. Import this leaf adapter only for side effects
// from an executable.
package autoload

import (
	"github.com/xbcio/xbc/extensions/reliability/automemlimit"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() {
	autoload.Declare(automemlimit.Bundle())
}
