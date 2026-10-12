package tracing

import (
	"errors"
	"strings"

	tracingcore "github.com/xbcio/xbc/extensions/observability/tracing"
	"github.com/xbcio/xbc/plugin"
	grpctransport "github.com/xbcio/xbc/transport/grpc"
)

const instrumentationName = "github.com/xbcio/xbc/transport/grpc/extensions/observability/tracing"

// Key is the stable Definition and interceptor identity of the gRPC tracing
// interceptor. It is distinct from the neutral tracing plugin's own key,
// tracing, which owns the plugins.tracing section: the interceptor has no
// configuration of its own and reads its correlation header settings from that
// plugin.
const Key plugin.Key = "tracing-grpc"

// metricsKey mirrors the stable plugin.Key identity owned by the sibling
// metrics interceptor. tracing must not import that module for a bare identity
// value (each optional integration is independently versioned).
const metricsKey plugin.Key = "grpc-metrics"

// Interceptor opens one server span per RPC on the neutral tracing plugin's
// private provider. Every pipeline decision -- sampling, export, propagation
// format -- stays with that plugin. One value contributes to both chains:
// unary and stream calls produce the same span shape.
type Interceptor struct {
	handle          tracingcore.Handle
	responseHeaders tracingcore.ResponseHeaders
}

var (
	_ grpctransport.UnaryInterceptor  = (*Interceptor)(nil)
	_ grpctransport.StreamInterceptor = (*Interceptor)(nil)
)

var tracingInput = plugin.RefTo[*tracingcore.Plugin](tracingcore.Key)

var definition = plugin.Define(
	Key,
	func(ctx plugin.BuildContext) (*Interceptor, error) {
		return New(tracingInput.Get(ctx).Value)
	},
	plugin.Options[*Interceptor]{
		// The interceptor follows the tracing plugin it pairs with: configuring
		// plugins.tracing turns both on, and leaving it out leaves both dormant.
		Activation: plugin.WhenConfigured("plugins." + tracingcore.Key.String()),
		Inputs:     plugin.Inputs(tracingInput),
		Exports: plugin.Contracts(
			plugin.ExportAs[grpctransport.UnaryInterceptor](func(value *Interceptor) grpctransport.UnaryInterceptor { return value }),
			plugin.ExportAs[grpctransport.StreamInterceptor](func(value *Interceptor) grpctransport.StreamInterceptor { return value }),
		),
	},
)

// bundle selects the neutral tracing plugin alongside the interceptor. An
// interceptor without the provider it records into is not a composition anyone
// wants, and RefTo would reject it at planning time anyway.
var bundle = plugin.CombineBundles(tracingcore.Bundle(), plugin.BundleOf(definition))

// New builds the gRPC tracing interceptor over the neutral tracing plugin.
func New(core *tracingcore.Plugin) (*Interceptor, error) {
	if core == nil {
		return nil, errors.New("tracing-grpc: requires the tracing plugin")
	}
	handle := core.Handle()
	if handle == nil {
		return nil, errors.New("tracing-grpc: the tracing plugin has no provider")
	}
	return newInterceptor(handle, core.ResponseHeaders()), nil
}

// newInterceptor lower-cases the correlation header names once, here: gRPC
// metadata keys are lower case, and the configured names are HTTP header
// spellings.
func newInterceptor(handle tracingcore.Handle, responseHeaders tracingcore.ResponseHeaders) *Interceptor {
	responseHeaders.TraceIDHeader = strings.ToLower(strings.TrimSpace(responseHeaders.TraceIDHeader))
	responseHeaders.SpanIDHeader = strings.ToLower(strings.TrimSpace(responseHeaders.SpanIDHeader))
	return &Interceptor{handle: handle, responseHeaders: responseHeaders}
}

// Definition returns the gRPC tracing interceptor's canonical immutable
// declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the side-effect-free explicit composition of the gRPC tracing
// interceptor and the neutral tracing plugin it records into.
func Bundle() plugin.Bundle { return bundle }
