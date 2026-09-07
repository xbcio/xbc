// Package autoload declares the securityheaders Bundle in XBC's optional
// process-wide composition. Import it only for side effects from an executable.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/web/extensions/security/securityheaders"
)

func init() {
	autoload.Declare(securityheaders.Bundle())
}
