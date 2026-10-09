package starter

import (
	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web/extensions/observability/accesslog"
	"github.com/xbcio/xbc/transport/web/extensions/observability/requestid"
	"github.com/xbcio/xbc/transport/web/extensions/reliability/timeout"
	"github.com/xbcio/xbc/transport/web/extensions/response/gzip"
	"github.com/xbcio/xbc/transport/web/extensions/security/securityheaders"
	"github.com/xbcio/xbc/transport/web/prelude"
)

// webBaselineLabel is how the starter's configuration layer is named in
// provenance output. It names the contributor rather than the layer kind --
// config pairs the two itself, so doctor prints "defaults (web starter)" --
// and it never carries a value.
const webBaselineLabel = "web starter"

// Web returns the baseline for a Web service: the prelude's Bundles, plus the
// configuration that turns on the capabilities among them which are dormant
// until a section exists.
//
// The returned value is stateless and safe to share; each call to Defaults
// builds its own map, so nothing an assembly does can mutate a shared
// baseline. Select it through WithStarter and select an engine Bundle
// separately.
func Web() plugin.Starter { return webBaseline{} }

type webBaseline struct{}

func (webBaseline) Bundles() []plugin.Bundle { return []plugin.Bundle{prelude.Bundle()} }

// Defaults turns on every prelude member whose Definition is WhenConfigured.
// The set is spelled member by member rather than derived from the Bundle,
// because the Bundle deliberately reports nothing about activation -- that
// separation is what keeps composition side-effect free, and this method is
// where the policy it leaves open is decided.
func (webBaseline) Defaults() config.Defaults {
	return config.Defaults{
		Label: webBaselineLabel,
		Values: map[string]any{
			activationPath(requestid.Key):       true,
			activationPath(accesslog.Key):       true,
			activationPath(securityheaders.Key): true,
			activationPath(gzip.Key):            true,
			activationPath(timeout.Key):         true,
		},
	}
}

// activationPath spells the configuration key that turns one capability on. It
// is derived from the member's own exported Key rather than written as a
// literal, so a renamed plugin fails to compile here instead of leaving behind
// a default that quietly activates nothing.
func activationPath(key plugin.Key) string {
	return "plugins." + key.String() + ".enabled"
}
