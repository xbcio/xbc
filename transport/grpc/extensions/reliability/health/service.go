package health

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	corehealth "github.com/xbcio/xbc/extensions/reliability/health"
)

// Check implements grpc_health_v1.HealthServer. It answers from a live probe:
// each call runs the aggregator's checks for the requested kind, so a
// dependency that failed since the previous probe is reported on the next one.
func (p *Plugin) Check(ctx context.Context, request *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	kind, err := kindForService(request.GetService())
	if err != nil {
		return nil, err
	}
	return &healthpb.HealthCheckResponse{Status: p.status(ctx, kind)}, nil
}

// Watch implements grpc_health_v1.HealthServer with UNIMPLEMENTED, as the
// protocol allows: a client that sees UNIMPLEMENTED must fall back to Check
// and not retry the watch. The neutral aggregator has no change notification
// to forward -- a push loop would have to poll and would then be answering
// with a staleness contract this adapter never promised -- so the honest
// answer is the one that tells the client to poll the live probe itself.
func (p *Plugin) Watch(*healthpb.HealthCheckRequest, grpc.ServerStreamingServer[healthpb.HealthCheckResponse]) error {
	return status.Error(codes.Unimplemented, "grpc health watch is not implemented; poll Check")
}

// List implements grpc_health_v1.HealthServer with UNIMPLEMENTED, matching
// Watch: the service names this adapter answers are the two probe kinds, and
// an enumeration of them adds nothing a Check against each name does not.
func (p *Plugin) List(context.Context, *healthpb.HealthListRequest) (*healthpb.HealthListResponse, error) {
	return nil, status.Error(codes.Unimplemented, "grpc health list is not implemented; probe the named services with Check")
}

// status computes one probe answer. A plugin whose prober or gate is absent
// fails closed: an adapter that cannot see the traffic gate cannot claim the
// instance is serving.
func (p *Plugin) status(ctx context.Context, kind corehealth.Kind) healthpb.HealthCheckResponse_ServingStatus {
	if p == nil || p.prober == nil || !p.trafficReleased() {
		return healthpb.HealthCheckResponse_NOT_SERVING
	}
	if p.prober.Check(ctx, kind).Healthy() {
		return healthpb.HealthCheckResponse_SERVING
	}
	return healthpb.HealthCheckResponse_NOT_SERVING
}

// trafficReleased reports whether the runtime has released the traffic gate.
// Before it does, whatever answers here is answered by something that was not
// supposed to admit traffic yet, so NOT_SERVING is the only truthful status.
func (p *Plugin) trafficReleased() bool {
	if p.gate == nil {
		return false
	}
	select {
	case <-p.gate:
		return true
	default:
		return false
	}
}

// kindForService maps the protocol's service name onto the aggregator's probe
// kind. The empty name -- what load balancers and grpc_health_probe dial by
// default -- means readiness, because the operative question for a load
// balancer is whether this instance should receive production work; liveness
// is the question a restart decision asks, and it is deliberately not the
// default, so a dependency outage cannot look like a reason to restart. Any
// other name is an unregistered service, which the protocol answers with
// NOT_FOUND rather than a guessed status.
func kindForService(service string) (corehealth.Kind, error) {
	switch service {
	case "":
		return corehealth.Readiness, nil
	case string(corehealth.Liveness):
		return corehealth.Liveness, nil
	case string(corehealth.Readiness):
		return corehealth.Readiness, nil
	default:
		return "", status.Errorf(codes.NotFound, "unknown health service %q; this server answers %q, %q, and the empty name",
			service, corehealth.Liveness, corehealth.Readiness)
	}
}
