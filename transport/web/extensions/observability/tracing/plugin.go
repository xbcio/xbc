package tracing

import (
	"errors"

	tracingcore "github.com/xbcio/xbc/extensions/observability/tracing"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

const instrumentationName = "github.com/xbcio/xbc/transport/web/extensions/observability/tracing"

// Key is the stable Definition and middleware identity of the HTTP tracing
// middleware. It is distinct from the neutral tracing plugin's own key,
// tracing, which owns the plugins.tracing section: the middleware has no
// configuration of its own and reads its correlation header settings from that
// plugin.
const Key plugin.Key = "tracing-http"

// metricsKey mirrors the stable plugin.Key identity owned by the sibling
// metrics middleware. tracing must not import that module for a bare identity
// value (each optional integration is independently versioned).
const metricsKey plugin.Key = "http-metrics"

// Middleware opens one server span per HTTP request on the neutral tracing
// plugin's private provider. Every pipeline decision -- sampling, export,
// propagation format -- stays with that plugin.
type Middleware struct {
	handle          tracingcore.Handle
	responseHeaders tracingcore.ResponseHeaders
}

var _ web.Middleware = (*Middleware)(nil)

var tracingInput = plugin.RefTo[*tracingcore.Plugin](tracingcore.Key)

var definition = plugin.Define(
	Key,
	func(ctx plugin.BuildContext) (*Middleware, error) {
		return New(tracingInput.Get(ctx).Value)
	},
	plugin.Options[*Middleware]{
		// The middleware follows the tracing plugin it pairs with: configuring
		// plugins.tracing turns both on, and leaving it out leaves both dormant.
		Activation: plugin.WhenConfigured("plugins." + tracingcore.Key.String()),
		Inputs:     plugin.Inputs(tracingInput),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.Middleware](func(value *Middleware) web.Middleware { return value }),
		),
	},
)

// bundle selects the neutral tracing plugin alongside the middleware. A
// middleware without the provider it records into is not a composition anyone
// wants, and RefTo would reject it at planning time anyway.
var bundle = plugin.CombineBundles(tracingcore.Bundle(), plugin.BundleOf(definition))

// New builds HTTP tracing middleware over the neutral tracing plugin.
func New(core *tracingcore.Plugin) (*Middleware, error) {
	if core == nil {
		return nil, errors.New("tracing-http: requires the tracing plugin")
	}
	handle := core.Handle()
	if handle == nil {
		return nil, errors.New("tracing-http: the tracing plugin has no provider")
	}
	return newMiddleware(handle, core.ResponseHeaders()), nil
}

func newMiddleware(handle tracingcore.Handle, responseHeaders tracingcore.ResponseHeaders) *Middleware {
	return &Middleware{handle: handle, responseHeaders: responseHeaders}
}

// Definition returns the HTTP tracing middleware's canonical immutable
// declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the side-effect-free explicit composition of the HTTP tracing
// middleware and the neutral tracing plugin it records into.
func Bundle() plugin.Bundle { return bundle }
