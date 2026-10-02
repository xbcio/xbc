// Package autoload declares async's canonical Bundle in XBC's optional
// process-wide composition. Import this leaf adapter only for side effects
// from an executable.
package autoload

import (
	"github.com/xbcio/xbc/extensions/concurrency/async"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() {
	autoload.Declare(async.Bundle())
}
