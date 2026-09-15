package gin

import (
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is the stable identity of the gin engine Plugin.
const Key plugin.Key = "web-engine-gin"

// definition's primary type is the concrete Factory, not the web.EngineFactory
// interface it implements: catalog validation requires every Definition's
// primary result type to be concrete. Exports declares the web.EngineFactory
// contract explicitly so web's plugin.RequireOne[web.EngineFactory]() input
// still resolves this Definition. It carries no configuration of its own:
// engine selection is a composition-root decision (which Bundle a service
// includes), not a runtime-configurable knob.
var definition = plugin.Define(
	Key,
	func(plugin.BuildContext) (Factory, error) {
		return Factory{}, nil
	},
	plugin.Options[Factory]{
		Exports: plugin.Contracts(
			plugin.ExportAs[web.EngineFactory](func(value Factory) web.EngineFactory { return value }),
		),
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
