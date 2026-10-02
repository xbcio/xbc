package gracefulshutdown

import "github.com/xbcio/xbc/plugin"

// Key is the stable configuration and runtime identity of the programmatic
// shutdown Controller.
const Key plugin.Key = "gracefulshutdown"

var definition = plugin.Define(
	Key,
	func(plugin.BuildContext) (*Controller, error) { return New(), nil },
	plugin.Options[*Controller]{
		Activation: plugin.WhenConfigured("plugins." + string(Key)),
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns this package's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns gracefulshutdown's side-effect-free explicit composition
// bundle.
func Bundle() plugin.Bundle { return bundle }
