package prelude

import (
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/extensions/observability/accesslog"
	"github.com/xbcio/xbc/transport/web/extensions/observability/requestid"
	"github.com/xbcio/xbc/transport/web/extensions/reliability/health"
	"github.com/xbcio/xbc/transport/web/extensions/reliability/recovery"
	"github.com/xbcio/xbc/transport/web/extensions/reliability/timeout"
	"github.com/xbcio/xbc/transport/web/extensions/response/gzip"
	"github.com/xbcio/xbc/transport/web/extensions/security/securityheaders"
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
