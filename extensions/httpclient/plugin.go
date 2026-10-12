package httpclient

import (
	"context"
	"errors"

	"github.com/xbcio/xbc/extensions/observability/metrics"
	"github.com/xbcio/xbc/extensions/observability/tracing"
	"github.com/xbcio/xbc/plugin"
)

// Key is this module's stable Definition and configuration identity. Each
// downstream is a named instance below plugins.http-client.<name>.
const Key plugin.Key = "http-client"

// tracerInput lets a composition that selected the tracing plugin record one
// client span per attempt -- with the propagation headers injected into that
// attempt's request -- without this module depending on the SDK.
//
// It is optional on purpose. An instance without a tracing stack makes its
// calls with no spans and no traceparent headers, and its dependency closure
// stays free of a telemetry stack the deployment did not choose.
var tracerInput = plugin.OptionalOne[tracing.Handle]()

// registererInput lets a composition that selected the metrics plugin publish
// this instance's request, duration, and retry series. Without one, nothing
// is registered and the zero-or-one query simply stays unbound.
var registererInput = plugin.OptionalOne[metrics.Registerer]()

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	func(ctx plugin.BuildContext, config Config) (*Client, error) {
		var tracer tracing.Handle
		if entry, ok := tracerInput.Get(ctx); ok {
			tracer = entry.Value
		}
		var registerer metrics.Registerer
		if entry, ok := registererInput.Get(ctx); ok {
			registerer = entry.Value
		}
		return newClient(ctx.Identity().Instance, config, tracer, registerer)
	},
	plugin.Options[*Client]{
		Instances:  plugin.MultipleInstances,
		Activation: plugin.WhenConfigured("plugins.http-client"),
		Inputs:     plugin.Inputs(tracerInput, registererInput),
		Lifecycle: plugin.Lifecycle[*Client]{
			Start:     reportOnStart,
			Preflight: reportOnValidate,
			Stop:      stopClient,
		},
	},
)

// Definition returns the canonical http-client declaration.
func Definition() plugin.Definition { return definition }

// Bundle returns the side-effect-free http-client composition.
func Bundle() plugin.Bundle { return plugin.BundleOf(definition) }

func prepareConfig(config Config) (Config, error) { return config.normalized() }

func stopClient(client *Client, ctx context.Context) error { return client.stop(ctx) }

// The endpoint report hangs off both Start and Preflight because the two run
// in disjoint phases: validate runs only Preflight and never Start, and boot
// runs only Start and never Preflight. One adapter alone would leave the
// other path silent. The line deliberately adds no identity fields -- the
// logger the context hands over is already scoped to the plugin key and the
// instance name -- so a JSON sink never sees a repeated key.
func reportOnStart(client *Client, ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("http-client: Start requires a plugin context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	client.reportEndpoint(ctx.Log())
	return nil
}

func reportOnValidate(client *Client, ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("http-client: Preflight requires a plugin context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	client.reportEndpoint(ctx.Log())
	return nil
}
