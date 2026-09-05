// Package autoload declares the biz Bundle in XBC's optional
// process-wide composition. Import it only for side effects from an executable.
package autoload

import (
	"github.com/xbcio/xbc/internal/autoload"
	"github.com/xbcio/xbc/transport/web/biz"
)

func init() {
	autoload.Declare(biz.Bundle())
}
