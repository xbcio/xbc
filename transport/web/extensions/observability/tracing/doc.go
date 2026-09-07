// Package tracing provides application-private OpenTelemetry tracing.
//
// # Usage
//
// Definition returns one canonical declaration whose sole primary value is
// *Plugin. The same value is exported under both the web.Middleware and
// *Handle contracts. Bundle is side-effect free and can be composed
// explicitly alongside an application Definition that requires the exported
// Handle:
//
//	var handleRef = plugin.RefTo[*tracing.Handle](tracing.Key)
//
//	type Publisher struct {
//		handle *tracing.Handle
//		tracer trace.Tracer
//	}
//
//	var publisherDefinition = plugin.Define(
//		"orders-publisher",
//		func(ctx plugin.BuildContext) (*Publisher, error) {
//			handle := handleRef.Get(ctx).Value
//			return &Publisher{handle: handle, tracer: handle.Tracer("example.com/orders/publisher")}, nil
//		},
//		plugin.Options[*Publisher]{Inputs: plugin.Inputs(handleRef)},
//	)
//
//	func (p *Publisher) Send(ctx context.Context, req *http.Request) (*http.Response, error) {
//		ctx, span := p.tracer.Start(ctx, "orders.publish")
//		defer span.End()
//
//		req = req.Clone(ctx)
//		if req.Header == nil {
//			req.Header = make(http.Header)
//		}
//		p.handle.Inject(ctx, propagation.HeaderCarrier(req.Header))
//		response, err := http.DefaultClient.Do(req)
//		if err != nil {
//			span.RecordError(err)
//		}
//		return response, err
//	}
//
// Each Plugin owns its TracerProvider and W3C propagator. It never replaces
// OpenTelemetry's process-wide provider or propagator; Handle.Extract and
// Handle.Inject always use this private pipeline. OTLP endpoints, credentials,
// and TLS material are validated from explicit plugin configuration rather
// than ambient exporter environment variables.
//
// The tracing plugin owns exporter cleanup. Because it exports web.Middleware
// — a construction-time input of web.Server — it always constructs before,
// and therefore stops after, the web server during reverse lifecycle unwind,
// so its bounded flush-and-shutdown runs only once dependent traffic has
// drained. Consumers should not independently call Handle.Shutdown during
// normal operation. Importing this implementation package has no composition
// side effects; import
// github.com/xbcio/xbc/transport/web/extensions/observability/tracing/autoload explicitly
// for default-bundle registration.
package tracing
