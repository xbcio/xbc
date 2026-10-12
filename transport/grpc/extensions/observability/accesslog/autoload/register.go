// Package autoload declares the gRPC access log's Bundle in XBC's optional
// process-wide composition. Import it only for side effects from an executable.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/grpc/extensions/observability/accesslog"
)

func init() {
	autoload.Declare(accesslog.Bundle())
}
