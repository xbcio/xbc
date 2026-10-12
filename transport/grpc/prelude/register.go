package prelude

import (
	"github.com/xbcio/xbc/plugin"
	grpctransport "github.com/xbcio/xbc/transport/grpc"
	"github.com/xbcio/xbc/transport/grpc/extensions/observability/accesslog"
	"github.com/xbcio/xbc/transport/grpc/extensions/reliability/health"
)

var bundle = plugin.CombineBundles(grpctransport.Bundle(), health.Bundle(), accesslog.Bundle())

// Bundle returns the side-effect-free gRPC baseline composition.
func Bundle() plugin.Bundle { return bundle }
