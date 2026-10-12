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
)

// Key is this plugin's stable configuration and dependency identity.
const Key plugin.Key = "tracing"

// Plugin owns one private SDK TracerProvider and propagator, built once from
// validated configuration when the Plugin is constructed.
type Plugin struct {
	config normalizedConfig
	handle *handle
}

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
	func(ctx plugin.BuildContext, cfg Config) (*Plugin, error) {
		return newPlugin(ctx.Service(), cfg, newOTLPExporter)
	},
	plugin.Options[*Plugin]{
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Exports: plugin.Contracts(
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
//
// It builds without a framework process identity, so the resource falls back
// to the OTLP convention's "unknown_service" unless cfg names one. Code
// assembled by an xbc application through Bundle goes through the Definition
// factory instead and inherits the process identity; New exists for callers
// embedding the plugin directly.
func New(cfg Config) (*Plugin, error) {
	return newPlugin(plugin.ServiceIdentity{}, cfg, newOTLPExporter)
}

func newPlugin(service plugin.ServiceIdentity, cfg Config, factory exporterFactory) (plug *Plugin, err error) {
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
		sdktrace.WithResource(buildResource(config, service)),
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

// ResponseHeaders returns the normalized trace-correlation response header
// settings from plugins.tracing.response_headers. A transport adapter that
// writes correlation headers reads them here rather than owning a
// configuration section of its own; when they are disabled the result is the
// zero value.
func (p *Plugin) ResponseHeaders() ResponseHeaders {
	if p == nil {
		return ResponseHeaders{}
	}
	return p.config.responseHeaders
}

// Shutdown implements Handle. It is the same bounded flush-and-shutdown Stop
// runs; expose it so a consumer holding the Handle contract can flush without
// owning the lifecycle.
func (p *Plugin) Shutdown(ctx context.Context) error { return p.contractHandle().Shutdown(ctx) }

// Stop force-flushes and shuts down the private provider. It is safe to call
// repeatedly and concurrently. Every transport adapter and span producer
// reaches this plugin through a construction-time input, so it constructs
// before and therefore stops after all of them during reverse lifecycle
// unwind, and this bounded flush-and-shutdown runs only once dependent
// traffic drains.
//
// Stop is the whole shutdown path: Plugin deliberately does not implement
// plugin.Drainer, for the reasons the package documentation's Shutdown section
// gives.
func (p *Plugin) Stop(ctx context.Context) error {
	return p.contractHandle().Shutdown(ctx)
}

// buildResource assembles the OTLP resource every span is stamped with. The
// service.name and service.version resolution order is: the deployment's
// explicit plugins.tracing value first, the framework's process identity
// (plugin.Context.Service) second -- so an application that names its service
// once, under xbc.service, gets matching logs, traces and metrics -- and the
// OTLP convention's "unknown_service" last, which only a direct New() call
// with no explicit name can reach. service.instance.id is stamped whenever
// the process identity carries one, which is every xbc-assembled process.
func buildResource(cfg normalizedConfig, service plugin.ServiceIdentity) *resource.Resource {
	keys := make([]string, 0, len(cfg.resourceAttributes))
	for key := range cfg.resourceAttributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	attributes := make([]attribute.KeyValue, 0, len(keys)+4)
	for _, key := range keys {
		attributes = append(attributes, attribute.String(key, cfg.resourceAttributes[key]))
	}
	name := cfg.serviceName
	if name == "" {
		name = service.Name
	}
	if name == "" {
		name = "unknown_service"
	}
	attributes = append(attributes, semconv.ServiceName(name))
	version := cfg.serviceVersion
	if version == "" {
		version = service.Version
	}
	if version != "" {
		attributes = append(attributes, semconv.ServiceVersion(version))
	}
	if service.Instance != "" {
		attributes = append(attributes, semconv.ServiceInstanceID(service.Instance))
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
