package gin

import (
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is the stable identity of the gin engine Plugin.
const Key plugin.Key = "web-engine-gin"

// definition exports Factory as the web.EngineFactory the web Plugin
// requires exactly one of. It carries no configuration of its own: engine
// selection is a composition-root decision (which Bundle a service
// includes), not a runtime-configurable knob.
var definition = plugin.Define(
	Key,
	func(plugin.BuildContext) (web.EngineFactory, error) {
		return Factory{}, nil
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns the canonical gin engine Plugin declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the gin engine as a selectable Plugin Bundle. A composition
// root that selects web.Bundle() must also select exactly one engine Bundle
// -- this one, or another Engine adapter -- because web.Server's Definition
// requires exactly one web.EngineFactory input.
func Bundle() plugin.Bundle { return bundle }
