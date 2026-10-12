package starter

import (
	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/grpc/extensions/observability/accesslog"
	"github.com/xbcio/xbc/transport/grpc/prelude"
)

// grpcBaselineLabel is how the starter's configuration layer is named in
// provenance output; doctor prints "defaults (grpc starter)".
const grpcBaselineLabel = "grpc starter"

// GRPC returns the baseline for a gRPC service: the prelude's Bundles, plus
// the configuration that turns on the capabilities among them which are
// dormant until a section exists.
//
// The returned value is stateless and safe to share; each call to Defaults
// builds its own map. Select it through WithStarter.
func GRPC() plugin.Starter { return grpcBaseline{} }

type grpcBaseline struct{}

func (grpcBaseline) Bundles() []plugin.Bundle { return []plugin.Bundle{prelude.Bundle()} }

// Defaults turns on every prelude member whose Definition is WhenConfigured.
// The set is spelled member by member rather than derived from the Bundle,
// because the Bundle deliberately reports nothing about activation; this
// method is where that policy is decided.
func (grpcBaseline) Defaults() config.Defaults {
	return config.Defaults{
		Label: grpcBaselineLabel,
		Values: map[string]any{
			activationPath(accesslog.Key): true,
		},
	}
}

// activationPath spells the configuration key that turns one capability on,
// derived from the member's exported Key so a rename fails to compile here.
func activationPath(key plugin.Key) string {
	return "plugins." + key.String() + ".enabled"
}
