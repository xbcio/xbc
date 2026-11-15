// Package autoload declares ginmount's canonical Bundle in XBC's
// optional process-global composition. Import it only for side effects from a
// process that wants the mount declared without listing the Bundle.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/web/extensions/interop/ginmount"
)

func init() { autoload.Declare(ginmount.Bundle()) }
