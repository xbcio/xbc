package webhook

import (
	"context"

	"github.com/xbcio/xbc/plugin"
)

// Key is the stable configuration and runtime identity.
const Key plugin.Key = "webhook"

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	func(_ plugin.BuildContext, config Config) (*Client, error) {
		return newClient(config, nil, nil, nil, nil), nil
	},
	plugin.Options[*Client]{
		Instances:  plugin.MultipleInstances,
		Activation: plugin.WhenConfigured("plugins.webhook"),
		Inputs:     plugin.Inputs(),
		Exports: plugin.Contracts(
			plugin.ExportAs(func(client *Client) Dispatcher { return client }),
		),
		Lifecycle: plugin.Lifecycle[*Client]{
			Start: startClient,
			Stop:  stopClient,
		},
	},
)

// Definition returns the canonical webhook client declaration.
func Definition() plugin.Definition { return definition }

// Bundle returns the side-effect-free webhook composition.
func Bundle() plugin.Bundle { return plugin.BundleOf(definition) }

func prepareConfig(config Config) (Config, error) { return config.normalized() }

func startClient(client *Client, ctx *plugin.Context) error { return client.start(ctx) }

func stopClient(client *Client, ctx context.Context) error { return client.stop(ctx) }
