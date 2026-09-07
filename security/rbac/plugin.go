package rbac

import "github.com/xbcio/xbc/plugin"

// Key is the stable Definition and configuration identity of the business
// RBAC Manager.
const Key plugin.Key = "rbac"

var definition = plugin.DefinePlanned(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	plan,
	plugin.Options[*manager]{
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs[Manager](func(value *manager) Manager { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

func plan(config Config) (plugin.Plan[*manager], error) {
	normalized, err := normalizeConfig(config)
	if err != nil {
		return plugin.Plan[*manager]{}, err
	}
	backendRef := plugin.RefToInstance[Backend](normalized.backend.Plugin, normalized.backend.Instance)
	return plugin.PlanOf(plugin.Inputs(backendRef), func(ctx plugin.BuildContext) (*manager, error) {
		entry := backendRef.Get(ctx)
		return newManager(normalized, entry.Identity, entry.Value)
	}), nil
}

// Definition returns rbac's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns rbac's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }
