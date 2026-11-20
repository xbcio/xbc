// Package autoload declares the mail integration in XBC's optional
// process-wide composition. Import it only for side effects from an executable.
package autoload

import (
	"github.com/xbcio/xbc/extensions/messaging/mailx"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() {
	autoload.Declare(mailx.Bundle())
}
