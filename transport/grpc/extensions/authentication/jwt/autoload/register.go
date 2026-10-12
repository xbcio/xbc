// Package autoload opts the gRPC jwt extractor's canonical Bundle into
// XBC's optional process-wide composition. Import this leaf adapter only for
// side effects from an executable.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/grpc/extensions/authentication/jwt"
)

func init() {
	autoload.Declare(jwt.Bundle())
}
