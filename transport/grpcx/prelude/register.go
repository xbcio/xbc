package prelude

import (
	"github.com/xbcio/xbc/plugin"
	grpctransport "github.com/xbcio/xbc/transport/grpcx"
	"github.com/xbcio/xbc/transport/grpcx/extensions/observability/accesslog"
	"github.com/xbcio/xbc/transport/grpcx/extensions/reliability/health"
)

var bundle = plugin.CombineBundles(grpctransport.Bundle(), health.Bundle(), accesslog.Bundle())

// Bundle returns the side-effect-free gRPC baseline composition.
func Bundle() plugin.Bundle { return bundle }
