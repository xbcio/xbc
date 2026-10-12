package tracing

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

// resourceSetFor builds a Plugin around the given framework identity,
// exports one span, and returns the resource set it was stamped with -- the
// same observable TestResourceContainsConfiguredServiceAttributes reads.
func resourceSetFor(t *testing.T, service plugin.ServiceIdentity, configure func(*Config)) *attribute.Set {
	t.Helper()
	exporter := new(recordingExporter)
	cfg := DefaultConfig()
	if configure != nil {
		configure(&cfg)
	}
	cfg.Batch.BatchTimeout = time.Hour
	p, err := newPlugin(service, cfg, func(context.Context, normalizedConfig) (sdktrace.SpanExporter, error) {
		return exporter, nil
	})
	if err != nil {
		t.Fatalf("newPlugin() error = %v", err)
	}
	t.Cleanup(func() {
		if err := p.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})

	_, span := p.Handle().Tracer("resource-test").Start(context.Background(), "resource")
	span.End()
	if err := p.Handle().ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	spans, _, _ := exporter.snapshot()
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	return spans[0].Resource.Set()
}

// TestResourceFallsBackToTheFrameworkServiceIdentity pins the back half of
// the tracing consumer: with no explicit plugins.tracing name or version, the
// resource carries the framework's process identity, so an application that
// names its service once under xbc.service gets matching log, trace and
// metric names -- and service.instance.id is stamped whenever the identity
// carries one, which every xbc-assembled process does.
func TestResourceFallsBackToTheFrameworkServiceIdentity(t *testing.T) {
	set := resourceSetFor(t, plugin.ServiceIdentity{Name: "orders", Version: "v1.2.3", Instance: "host-7-123"}, nil)

	assertResourceString(t, set, semconv.ServiceNameKey, "orders")
	assertResourceString(t, set, semconv.ServiceVersionKey, "v1.2.3")
	assertResourceString(t, set, attribute.Key("service.instance.id"), "host-7-123")
}

// TestDefinitionFactoryCarriesTheFrameworkIdentity pins the one seam this
// file's resourceSetFor tests cannot see: the Definition factory itself must
// hand newPlugin the identity a BuildContext carries. Those tests inject the
// identity straight into newPlugin, so a factory passing a zero identity
// instead would leave every one of them green while every assembled
// application silently lost its trace identity. This test therefore builds
// through the real planning and construction path -- BuildPlan and Construct,
// the same machinery a boot runs -- and reads the resource off the span the
// constructed provider actually stamps.
func TestDefinitionFactoryCarriesTheFrameworkIdentity(t *testing.T) {
	environment, err := config.NewEnvironment(
		map[string]any{"plugins": map[string]any{Key.String(): map[string]any{}}},
		"XBC_TRACING_DEFINITION_TEST_UNSET_")
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle()},
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	constructed, err := assembly.Construct(plan, assembly.ConstructOptions{
		Service: plugin.ServiceIdentity{Name: "orders", Version: "v1.2.3", Instance: "host-7-123"},
	})
	if err != nil {
		t.Fatalf("Construct: %v", err)
	}
	instance, ok := constructed.Instance(plugin.Identity{Plugin: Key})
	if !ok {
		t.Fatal("the tracing instance was not constructed")
	}
	p, ok := instance.Primary().(*Plugin)
	if !ok {
		t.Fatalf("primary = %T, want *Plugin", instance.Primary())
	}
	t.Cleanup(func() {
		if err := p.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})

	// The resource is read off a live span rather than an exported one: an
	// assembled Plugin uses the real OTLP exporter, and ending a span would
	// queue an export to a collector this test does not run.
	_, span := p.Tracer("definition-test").Start(context.Background(), "identity probe")
	readOnly, ok := span.(sdktrace.ReadOnlySpan)
	if !ok {
		t.Fatalf("span = %T, want an SDK span implementing sdktrace.ReadOnlySpan", span)
	}
	set := readOnly.Resource().Set()

	assertResourceString(t, set, semconv.ServiceNameKey, "orders")
	assertResourceString(t, set, semconv.ServiceVersionKey, "v1.2.3")
	assertResourceString(t, set, attribute.Key("service.instance.id"), "host-7-123")
}

// TestExplicitTracingIdentityBeatsTheFrameworkIdentity pins the front half:
// an explicit plugins.tracing name or version still wins, because a
// deployment overriding the tracing resource keeps that override. The
// instance has no explicit counterpart and always comes from the framework.
func TestExplicitTracingIdentityBeatsTheFrameworkIdentity(t *testing.T) {
	set := resourceSetFor(t, plugin.ServiceIdentity{Name: "orders", Version: "v1.2.3", Instance: "host-7-123"}, func(c *Config) {
		c.ServiceName = "checkout"
		c.ServiceVersion = "2.4.1"
	})

	assertResourceString(t, set, semconv.ServiceNameKey, "checkout")
	assertResourceString(t, set, semconv.ServiceVersionKey, "2.4.1")
	assertResourceString(t, set, attribute.Key("service.instance.id"), "host-7-123")
}

// TestResourceWithNoIdentityAnywhereIsTheOtlpUnknownService pins the last
// link of the chain for a direct New() call with no explicit name: the OTLP
// convention's "unknown_service", never the empty string -- and no
// service.instance.id, because nothing recorded one.
func TestResourceWithNoIdentityAnywhereIsTheOtlpUnknownService(t *testing.T) {
	set := resourceSetFor(t, plugin.ServiceIdentity{}, nil)

	assertResourceString(t, set, semconv.ServiceNameKey, "unknown_service")
	if got, ok := set.Value(semconv.ServiceVersionKey); ok {
		t.Fatalf("resource carries service.version %v although nothing recorded one", got)
	}
	if got, ok := set.Value(attribute.Key("service.instance.id")); ok {
		t.Fatalf("resource carries service.instance.id %v although nothing recorded one", got)
	}
}
