// Package autoload declares the gRPC health adapter's Bundle in XBC's optional
// process-wide composition. Import it only for side effects from an executable.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/grpc/extensions/reliability/health"
)

func init() {
	autoload.Declare(health.Bundle())
}
