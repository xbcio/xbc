package health

import (
	"context"
	"errors"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	corehealth "github.com/xbcio/xbc/extensions/reliability/health"
	"github.com/xbcio/xbc/plugin"
	grpctransport "github.com/xbcio/xbc/transport/grpc"
)

// Key is the stable Definition identity of the gRPC health adapter. It is
// distinct from the neutral capability's own key, health, which owns the
// plugins.health section: the adapter has no configuration of its own and
// reads its probe policy from that plugin.
const Key plugin.Key = "health-grpc"

// healthServiceName is the gRPC service name of the standard health protocol
// this adapter registers and declares.
const healthServiceName = "grpc.health.v1.Health"

// prober is the seam this adapter renders. The neutral *corehealth.Plugin
// satisfies it; declaring it locally keeps the aggregator's constructor out of
// this package's tests without widening the capability's public surface.
type prober interface {
	Check(context.Context, corehealth.Kind) corehealth.Report
}

// Plugin serves the standard gRPC health service for the neutral health
// aggregator. It owns no checks, no aggregation, and no probe policy.
//
// The traffic gate is captured in Init, like the neutral aggregator captures
// the execution context: both facts are read on every probe, and both are
// written once by the runtime before any RPC can arrive.
type Plugin struct {
	prober prober
	gate   <-chan struct{}
}

var (
	_ grpctransport.ServiceRegistrar = (*Plugin)(nil)
	_ plugin.Initializer             = (*Plugin)(nil)
	_ healthpb.HealthServer          = (*Plugin)(nil)
)

var proberInput = plugin.RefTo[*corehealth.Plugin](corehealth.Key)

// The adapter is deliberately always enabled rather than tied to
// plugins.health: probes are a baseline, and the aggregator it renders is
// always enabled too, with its own defaults when no section is written. A
// service that does not want the health service simply does not select this
// Bundle.
var definition = plugin.Define(
	Key,
	func(ctx plugin.BuildContext) (*Plugin, error) {
		return New(proberInput.Get(ctx).Value)
	},
	plugin.Options[*Plugin]{
		Inputs: plugin.Inputs(proberInput),
		Exports: plugin.Contracts(
			plugin.ExportAs[grpctransport.ServiceRegistrar](func(value *Plugin) grpctransport.ServiceRegistrar { return value }),
		),
	},
)

// bundle selects the neutral capability alongside the adapter. Serving the
// health service without an aggregator behind it is not a composition anyone
// wants, and RefTo would reject it at planning time anyway.
var bundle = plugin.CombineBundles(corehealth.Bundle(), plugin.BundleOf(definition))

// New constructs the adapter over the neutral health aggregator. Its probe
// policy -- per-check timeouts, the readiness shutdown rule -- stays with that
// plugin.
func New(core *corehealth.Plugin) (*Plugin, error) {
	if core == nil {
		return nil, errors.New("health-grpc: requires the health aggregator")
	}
	return newPlugin(core), nil
}

func newPlugin(aggregator prober) *Plugin {
	return &Plugin{prober: aggregator}
}

// Init captures the runtime traffic gate. Before the gate is released the
// health service must answer NOT_SERVING, and a plugin that skipped Init has
// no gate to observe, so its probes fail closed to the same answer.
func (p *Plugin) Init(ctx *plugin.Context) error {
	if p == nil {
		return errors.New("health-grpc: Init requires a non-nil plugin")
	}
	if ctx == nil {
		return errors.New("health-grpc: Init requires a non-nil plugin context")
	}
	p.gate = ctx.TrafficGate()
	return nil
}

// RegisterServices implements grpctransport.ServiceRegistrar. The health
// service is declared public -- a probe that depends on credentials cannot
// answer whether the instance is serving, which is why the Web adapter's
// probe routes are public too -- and the declaration is registration-time, so
// a grpc.security rule still outranks it.
func (p *Plugin) RegisterServices(r *grpctransport.Registrar) error {
	if p == nil {
		return errors.New("health-grpc: RegisterServices requires a non-nil plugin")
	}
	healthpb.RegisterHealthServer(r, p)
	r.Methods(healthServiceName).Auth(grpctransport.Public())
	return nil
}

// Definition returns the adapter's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the side-effect-free explicit composition of the gRPC health
// adapter and the neutral health capability it renders.
func Bundle() plugin.Bundle { return bundle }
