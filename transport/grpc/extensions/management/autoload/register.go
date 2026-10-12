// Package autoload declares the gRPC management plane's Bundle in XBC's
// optional process-wide composition. Import it only for side effects from an
// executable.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/grpc/extensions/management"
)

func init() {
	autoload.Declare(management.Bundle())
}
