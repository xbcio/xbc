package tracing

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

const instrumentationName = "github.com/xbcio/xbc/transport/web/extensions/observability/tracing"

// Key is this plugin's stable configuration, dependency, and middleware
// identity.
const Key plugin.Key = "tracing"

// metricsKey mirrors the stable plugin.Key identity owned by the sibling
// extensions/observability/metrics plugin. tracing must not import that module for a bare
// identity value (each optional integration is independently versioned).
const metricsKey plugin.Key = "metrics"

// Plugin owns one private SDK TracerProvider and propagator, built once from
// validated configuration when the Plugin is constructed.
type Plugin struct {
	config normalizedConfig
	handle *handle
}

var _ web.Middleware = (*Plugin)(nil)

// Plugin is the primary value the Definition constructs, so it is also the
// value the Handle contract is satisfied by: the framework requires a
// contract's interface to be assignable from that primary type. The facade
// methods below forward to the private handle, which keeps the SDK and OTLP
// implementation unreachable even though the contract is exposed on Plugin.
var _ Handle = (*Plugin)(nil)

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	func(_ plugin.BuildContext, cfg Config) (*Plugin, error) {
		return newPlugin(cfg, newOTLPExporter)
	},
	plugin.Options[*Plugin]{
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.Middleware](func(value *Plugin) web.Middleware { return value }),
			plugin.ExportAs[Handle](func(value *Plugin) Handle { return value }),
		),
		// No Lifecycle adapter for Stop: *Plugin satisfies plugin.Closer
		// directly, and the framework rejects a Definition that declares an
		// adapter for a stage its primary type already implements.
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns tracing's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns tracing's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }

// New constructs a directly usable Plugin from cfg. The exporter, batch
// processor, and TracerProvider are built immediately; a configuration or
// exporter-startup error is returned rather than deferred to a later stage.
// Building the OTLP client is not a blocking network operation, so eager
// construction here never risks stalling application startup.
func New(cfg Config) (*Plugin, error) {
	return newPlugin(cfg, newOTLPExporter)
}

func newPlugin(cfg Config, factory exporterFactory) (plug *Plugin, err error) {
	config, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	if factory == nil {
		factory = newOTLPExporter
	}
	exporter, err := factory(context.Background(), config)
	if err != nil {
		if exporter != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), config.batch.ExportTimeout)
			cleanupErr := exporter.Shutdown(cleanupCtx)
			cancel()
			err = errors.Join(err, cleanupErr)
		}
		return nil, fmt.Errorf("tracing: create exporter: %w", err)
	}
	if exporter == nil {
		return nil, fmt.Errorf("tracing: exporter factory returned nil")
	}

	// Keep a stage-appropriate rollback function armed until construction
	// succeeds. Before the processor exists the exporter is closed directly;
	// afterward the processor/provider/handle owns exporter cleanup.
	rollback := exporter.Shutdown
	owned := true
	defer func() {
		if !owned {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), config.batch.ExportTimeout)
		cleanupErr := rollback(cleanupCtx)
		cancel()
		err = errors.Join(err, cleanupErr)
	}()

	processorOptions := []sdktrace.BatchSpanProcessorOption{
		sdktrace.WithMaxQueueSize(config.batch.MaxQueueSize),
		sdktrace.WithMaxExportBatchSize(config.batch.MaxExportBatchSize),
		sdktrace.WithBatchTimeout(config.batch.BatchTimeout),
		sdktrace.WithExportTimeout(config.batch.ExportTimeout),
	}
	if config.batch.BlockOnQueueFull {
		processorOptions = append(processorOptions, sdktrace.WithBlocking())
	}
	processor := sdktrace.NewBatchSpanProcessor(exporter, processorOptions...)
	rollback = processor.Shutdown
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(config.sampleRatio))),
		sdktrace.WithResource(buildResource(config)),
		sdktrace.WithSpanProcessor(processor),
	)
	handle := newHandle(provider, buildPropagator(config.propagation), provider.ForceFlush, provider.Shutdown, config.batch.ExportTimeout)
	rollback = handle.Shutdown

	owned = false
	return &Plugin{config: config, handle: handle}, nil
}

// contractHandle returns the private implementation behind the Handle facade,
// tolerating a Plugin that newPlugin did not build. Every facade method goes
// through it, so a zero Plugin degrades to the same no-op answers a nil
// *handle already gives rather than panicking.
func (p *Plugin) contractHandle() *handle {
	if p == nil {
		return nil
	}
	return p.handle
}

// Handle returns this plugin's private tracing contract.
//
// A Plugin built by newPlugin always has one. A zero Plugin does not, and this
// returns a nil interface for it rather than an interface wrapping a nil
// *handle, so a caller's nil test reports the absence truthfully.
func (p *Plugin) Handle() Handle {
	if p == nil || p.handle == nil {
		return nil
	}
	return p.handle
}

// Provider implements Handle.
func (p *Plugin) Provider() trace.TracerProvider { return p.contractHandle().Provider() }

// Tracer implements Handle.
func (p *Plugin) Tracer(name string, options ...trace.TracerOption) trace.Tracer {
	return p.contractHandle().Tracer(name, options...)
}

// Extract implements Handle.
func (p *Plugin) Extract(ctx context.Context, carrier propagation.TextMapCarrier) context.Context {
	return p.contractHandle().Extract(ctx, carrier)
}

// Inject implements Handle.
func (p *Plugin) Inject(ctx context.Context, carrier propagation.TextMapCarrier) {
	p.contractHandle().Inject(ctx, carrier)
}

// ForceFlush implements Handle.
func (p *Plugin) ForceFlush(ctx context.Context) error { return p.contractHandle().ForceFlush(ctx) }

// Shutdown implements Handle. It is the same bounded flush-and-shutdown Stop
// runs; expose it so a consumer holding the Handle contract can flush without
// owning the lifecycle.
func (p *Plugin) Shutdown(ctx context.Context) error { return p.contractHandle().Shutdown(ctx) }

// Stop force-flushes and shuts down the private provider. It is safe to call
// repeatedly and concurrently. Because tracing exports web.Middleware, a
// construction-time input of web.Server, it always constructs before and
// therefore stops after the web server during reverse lifecycle unwind, so
// this bounded flush-and-shutdown runs only once dependent traffic drains.
//
// Stop is the whole shutdown path: Plugin deliberately does not implement
// plugin.Drainer, for the reasons the package documentation's Shutdown section
// gives.
func (p *Plugin) Stop(ctx context.Context) error {
	return p.contractHandle().Shutdown(ctx)
}

func buildResource(cfg normalizedConfig) *resource.Resource {
	keys := make([]string, 0, len(cfg.resourceAttributes))
	for key := range cfg.resourceAttributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	attributes := make([]attribute.KeyValue, 0, len(keys)+3)
	for _, key := range keys {
		attributes = append(attributes, attribute.String(key, cfg.resourceAttributes[key]))
	}
	attributes = append(attributes, semconv.ServiceName(cfg.serviceName))
	if cfg.serviceVersion != "" {
		attributes = append(attributes, semconv.ServiceVersion(cfg.serviceVersion))
	}
	if cfg.environment != "" {
		attributes = append(attributes, semconv.DeploymentEnvironmentNameKey.String(cfg.environment))
	}
	return resource.NewWithAttributes(semconv.SchemaURL, attributes...)
}

func buildPropagator(names []string) propagation.TextMapPropagator {
	propagators := make([]propagation.TextMapPropagator, 0, len(names))
	for _, name := range names {
		switch name {
		case "tracecontext":
			propagators = append(propagators, propagation.TraceContext{})
		case "baggage":
			propagators = append(propagators, propagation.Baggage{})
		}
	}
	return propagation.NewCompositeTextMapPropagator(propagators...)
}
