package management

import (
	"github.com/prometheus/client_golang/prometheus"

	metricscore "github.com/xbcio/xbc/extensions/observability/metrics"
	"github.com/xbcio/xbc/plugin"
)

// Key is the stable Definition identity of the gRPC management plane. It is
// distinct from the neutral metrics plugin's own key, metrics: this plugin
// owns the plugins.grpc-management section -- the listener -- while the
// exposition it serves belongs to that plugin's registry.
const Key plugin.Key = "grpc-management"

// metricsInput is the optional source of the exposition. It is optional
// because a deployment may configure the management listener for pprof alone:
// with no metrics plugin selected, /metrics is not registered at all, so a
// scrape cannot read an empty registry as "no metrics are being recorded".
var metricsInput = plugin.OptionalOne[*metricscore.Plugin]()

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  normalizeConfig,
	},
	func(ctx plugin.BuildContext, cfg Config) (*Plugin, error) {
		var gatherer prometheus.Gatherer
		if entry, found := metricsInput.Get(ctx); found {
			// A selected metrics plugin without a registry is a construction
			// error it should have raised itself; treating it as "no metrics
			// selected" keeps this listener from being the second place that
			// fails, and leaves the exposition absent rather than empty.
			if registry := entry.Value.Registry(); registry != nil {
				gatherer = registry
			}
		}
		return newPlugin(cfg, gatherer), nil
	},
	plugin.Options[*Plugin]{
		// The section's presence is the activation: an operator who wrote
		// plugins.grpc-management asked for a management listener, and an
		// address is required with it.
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Inputs:     plugin.Inputs(metricsInput),
	},
)

// bundle selects the neutral metrics plugin alongside the management plane.
// The exposition is a rendering of that plugin's registry, and selecting
// metrics is how the /metrics route comes to exist; a deployment that wants
// the listener for pprof alone is still free to operate it without the
// metrics plugin, because the Bundle is a default composition rather than a
// requirement -- both halves stay independently selectable.
var bundle = plugin.CombineBundles(metricscore.Bundle(), plugin.BundleOf(definition))

// New constructs the management plugin over an optional gatherer. A nil
// gatherer means /metrics is not served; pass the neutral metrics plugin's
// registry to expose it, which is what the Definition does when that plugin is
// selected. The configuration is normalized and validated here, so a
// programmatic caller gets the same refusals the configured plugin does.
func New(cfg Config, gatherer prometheus.Gatherer) (*Plugin, error) {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	return newPlugin(normalized, gatherer), nil
}

// newPlugin assembles a plugin from an already normalized configuration.
func newPlugin(cfg Config, gatherer prometheus.Gatherer) *Plugin {
	return &Plugin{
		config:   cfg,
		gatherer: gatherer,
		handler:  newHandler(cfg, gatherer),
	}
}

// Definition returns the management plane's canonical immutable declaration
// handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the side-effect-free explicit composition of the management
// plane and the neutral metrics plugin whose registry it exposes.
func Bundle() plugin.Bundle { return bundle }
