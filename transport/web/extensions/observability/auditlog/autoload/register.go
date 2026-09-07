// Package autoload declares the auditlog Bundle in XBC's optional
// process-wide composition. Import it only for side effects from an executable.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/web/extensions/observability/auditlog"
)

func init() {
	autoload.Declare(auditlog.Bundle())
}
