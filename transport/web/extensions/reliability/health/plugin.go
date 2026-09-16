package health

import (
	"context"
	"errors"

	corehealth "github.com/xbcio/xbc/extensions/reliability/health"
	"github.com/xbcio/xbc/plugin"
	transportweb "github.com/xbcio/xbc/transport/web"
)

// Key is the stable configuration and runtime identity of the HTTP probe
// adapter. It is distinct from the neutral capability's own key because the two
// are independently selected products with separate configuration sections: a
// background-only service composes the aggregator without any HTTP surface.
const Key plugin.Key = "health-http"

// prober is the seam this adapter renders. The neutral *corehealth.Plugin
// satisfies it; declaring it locally keeps the aggregator's constructor out of
// this package's tests without widening the capability's public surface.
type prober interface {
	Check(context.Context, corehealth.Kind) corehealth.Report
}

// Plugin serves the liveness and readiness endpoints for the neutral health
// aggregator. It owns no checks, no aggregation, and no probe policy.
type Plugin struct {
	cfg    Config
	prober prober
}

var _ transportweb.RouteContributor = (*Plugin)(nil)

var proberInput = plugin.RefTo[*corehealth.Plugin](corehealth.Key)

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	func(ctx plugin.BuildContext, cfg Config) (*Plugin, error) {
		return newPlugin(cfg, proberInput.Get(ctx).Value)
	},
	plugin.Options[*Plugin]{
		Inputs: plugin.Inputs(proberInput),
		Exports: plugin.Contracts(
			plugin.ExportAs[transportweb.RouteContributor](func(value *Plugin) transportweb.RouteContributor { return value }),
		),
	},
)

// bundle selects the neutral capability alongside the adapter. Serving a probe
// endpoint without an aggregator behind it is not a composition anyone wants,
// and RefTo would reject it at planning time anyway.
var bundle = plugin.CombineBundles(corehealth.Bundle(), plugin.BundleOf(definition))

// Definition returns the HTTP probe adapter's canonical immutable declaration
// handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the side-effect-free explicit composition of the HTTP probe
// adapter and the neutral health capability it renders.
func Bundle() plugin.Bundle { return bundle }

func prepareConfig(cfg Config) (Config, error) {
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func newPlugin(cfg Config, aggregator prober) (*Plugin, error) {
	prepared, err := prepareConfig(cfg)
	if err != nil {
		return nil, err
	}
	if aggregator == nil {
		return nil, errors.New("health-http: requires the health aggregator")
	}
	return &Plugin{cfg: prepared, prober: aggregator}, nil
}
