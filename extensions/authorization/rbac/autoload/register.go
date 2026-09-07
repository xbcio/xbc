// Package autoload declares rbac's canonical Bundle in XBC's optional
// process-wide composition. Import this leaf adapter only for side effects from
// an executable.
package autoload

import (
	"github.com/xbcio/xbc/extensions/authorization/rbac"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() {
	autoload.Declare(rbac.Bundle())
}
