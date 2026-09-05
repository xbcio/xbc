package prelude

import (
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/accesslog"
	"github.com/xbcio/xbc/transport/web/gzip"
	"github.com/xbcio/xbc/transport/web/health"
	"github.com/xbcio/xbc/transport/web/recovery"
	"github.com/xbcio/xbc/transport/web/requestid"
	"github.com/xbcio/xbc/transport/web/securityheaders"
	"github.com/xbcio/xbc/transport/web/timeout"
)

var bundle = plugin.CombineBundles(
	web.Bundle(),
	recovery.Bundle(),
	requestid.Bundle(),
	accesslog.Bundle(),
	securityheaders.Bundle(),
	gzip.Bundle(),
	timeout.Bundle(),
	health.Bundle(),
)

// Bundle returns the side-effect-free lightweight Web baseline. It combines
// the members' canonical Bundles without copying Definitions or changing their
// activation policies.
func Bundle() plugin.Bundle { return bundle }
