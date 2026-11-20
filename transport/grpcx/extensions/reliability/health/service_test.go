package health

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	corehealth "github.com/xbcio/xbc/extensions/reliability/health"
)

// fakeProber records the kinds it was asked for and answers with a fixed
// status, so a test can tell which probe kind a request resolved to and
// whether the prober ran at all.
type fakeProber struct {
	status corehealth.Status
	kinds  []corehealth.Kind
}

func (f *fakeProber) Check(_ context.Context, kind corehealth.Kind) corehealth.Report {
	f.kinds = append(f.kinds, kind)
	return corehealth.Report{Kind: kind, Status: f.status}
}

func releasedGate() <-chan struct{} {
	gate := make(chan struct{})
	close(gate)
	return gate
}

func TestCheckResolvesServiceNamesToProbeKinds(t *testing.T) {
	cases := []struct {
		service string
		want    corehealth.Kind
	}{
		{service: "", want: corehealth.Readiness},
		{service: "readiness", want: corehealth.Readiness},
		{service: "liveness", want: corehealth.Liveness},
	}
	for _, testCase := range cases {
		t.Run("service="+testCase.service, func(t *testing.T) {
			prober := &fakeProber{status: corehealth.Up}
			p := newPlugin(prober)
			p.gate = releasedGate()

			response, err := p.Check(context.Background(), &healthpb.HealthCheckRequest{Service: testCase.service})
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if response.GetStatus() != healthpb.HealthCheckResponse_SERVING {
				t.Fatalf("status = %v, want SERVING", response.GetStatus())
			}
			if want := []corehealth.Kind{testCase.want}; !slices.Equal(prober.kinds, want) {
				t.Fatalf("prober kinds = %v, want %v", prober.kinds, want)
			}
		})
	}
}

func TestCheckReportsDownAsNotServing(t *testing.T) {
	p := newPlugin(&fakeProber{status: corehealth.Down})
	p.gate = releasedGate()

	response, err := p.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if response.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("status = %v, want NOT_SERVING", response.GetStatus())
	}
}

// TestCheckAnswersNotServingBeforeTheTrafficGateReleases pins the gate rule:
// while the runtime has not released traffic, the probe answers NOT_SERVING
// without consulting the aggregator, because a "serving" answer from an
// instance that has not been admitted must not be possible.
func TestCheckAnswersNotServingBeforeTheTrafficGateReleases(t *testing.T) {
	prober := &fakeProber{status: corehealth.Up}
	p := newPlugin(prober)
	gate := make(chan struct{})
	p.gate = gate

	response, err := p.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if response.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("status before gate release = %v, want NOT_SERVING", response.GetStatus())
	}
	if len(prober.kinds) != 0 {
		t.Fatalf("prober ran before gate release: kinds = %v", prober.kinds)
	}

	close(gate)
	response, err = p.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Check() after release error = %v", err)
	}
	if response.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("status after gate release = %v, want SERVING", response.GetStatus())
	}
}

// TestCheckFailsClosedWithoutAGateOrProber covers the directly constructed
// value: an adapter that never saw Init, and one built without an aggregator,
// both answer NOT_SERVING rather than claiming health they cannot observe.
func TestCheckFailsClosedWithoutAGateOrProber(t *testing.T) {
	withoutGate := newPlugin(&fakeProber{status: corehealth.Up})
	response, err := withoutGate.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if response.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("status without gate = %v, want NOT_SERVING", response.GetStatus())
	}

	withoutProber := &Plugin{gate: releasedGate()}
	response, err = withoutProber.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if response.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("status without prober = %v, want NOT_SERVING", response.GetStatus())
	}
}

func TestCheckRejectsUnknownServicesWithNotFound(t *testing.T) {
	p := newPlugin(&fakeProber{status: corehealth.Up})
	p.gate = releasedGate()

	_, err := p.Check(context.Background(), &healthpb.HealthCheckRequest{Service: "greeter.v1.Greeter"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("unknown service code = %v, want NotFound (error %v)", status.Code(err), err)
	}
}

func TestWatchAndListAnswerUnimplemented(t *testing.T) {
	p := newPlugin(&fakeProber{status: corehealth.Up})
	p.gate = releasedGate()

	if err := p.Watch(&healthpb.HealthCheckRequest{}, nil); status.Code(err) != codes.Unimplemented {
		t.Fatalf("Watch() code = %v, want Unimplemented (error %v)", status.Code(err), err)
	}
	if _, err := p.List(context.Background(), &healthpb.HealthListRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("List() code = %v, want Unimplemented (error %v)", status.Code(err), err)
	}
}
