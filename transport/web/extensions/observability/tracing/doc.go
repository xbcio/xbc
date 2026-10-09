// Package tracing provides application-private OpenTelemetry tracing.
//
// # Usage
//
// Definition returns one canonical declaration whose sole primary value is
// *Plugin. The same value is exported under both the web.Middleware and
// Handle contracts. Bundle is side-effect free and can be composed
// explicitly alongside an application Definition that requires the exported
// Handle:
//
//	var handleRef = plugin.RefTo[tracing.Handle](tracing.Key)
//
//	type Publisher struct {
//		handle tracing.Handle
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
//
// # Shutdown
//
// Plugin implements Stop only, not plugin.Drainer, and that is deliberate
// rather than an oversight even though the spans it accepts are buffered
// asynchronously: Stop force-flushes that buffer before releasing the exporter,
// so work accepted before shutdown is exported rather than dropped. Drain
// would add no safety beyond it. Every span producer holds the Handle contract,
// which means it constructs after this plugin and is therefore stopped before
// it, so no producer can be recording while Stop runs; and the flush needs no
// shared shutdown budget to protect a dependent, because the only dependent
// that could still need the exporter is the exporter's own owner. What Stop
// does share with a drain phase is that it runs the flush detached from the
// caller's deadline, under the configured batch export timeout, once and only
// once no matter how many callers arrive -- a caller whose own context expires
// gets back its context error without cutting the flush short.
package tracing
