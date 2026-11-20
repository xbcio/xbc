// Package autoload declares the gRPC casbin authorization interceptor's
// Bundle in XBC's optional process-wide composition. Import it only for side
// effects from an executable.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/grpcx/extensions/authorization/casbinx"
)

func init() {
	autoload.Declare(casbinx.Bundle())
}
