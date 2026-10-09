package health

import (
	"context"
	"net/http"
	"time"

	corehealth "github.com/xbcio/xbc/extensions/reliability/health"
	transportweb "github.com/xbcio/xbc/transport/web"
)

// RegisterRoutes implements web.RouteContributor.
//
// Probe endpoints explicitly bypass authentication so operational health never
// depends on credentials, and they are unmetered so admission control never
// answers them either. The second exemption matters as much as the first: the
// in-flight ceiling exists to refuse work when the process is saturated, and a
// liveness probe is not work. Without it, a busy process fails its own probe
// and is restarted by its orchestrator at exactly the moment it is carrying its
// full traffic ceiling, moving that traffic onto replicas that then fail their
// probes in turn. An orchestrator cannot distinguish "saturated" from "dead"
// unless the probe is answered, and the two call for opposite responses.
//
// The cost of the exemption is that an unmetered probe is bounded by nothing
// but its own checks, which is why every check declares a timeout and the
// aggregator runs them concurrently: see the report this handler renders.
func (p *Plugin) RegisterRoutes(router *transportweb.Router) {
	router.GET(p.cfg.LivenessPath, p.probeHandler(corehealth.Liveness)).Unmetered().Auth(transportweb.Public())
	router.GET(p.cfg.ReadinessPath, p.probeHandler(corehealth.Readiness)).Unmetered().Auth(transportweb.Public())
}

func (p *Plugin) probeHandler(kind corehealth.Kind) transportweb.Handler {
	return func(ctx context.Context, c *transportweb.Ctx) error {
		report := p.prober.Check(ctx, kind)
		statusCode := http.StatusOK
		if !report.Healthy() {
			statusCode = http.StatusServiceUnavailable
		}
		c.JSON(statusCode, renderReport(report, p.cfg.DetailPolicy))
		return nil
	}
}

type webReport struct {
	Status corehealth.Status `json:"status"`
	Probe  corehealth.Kind   `json:"probe"`
	Checks []webResult       `json:"checks"`
}

type webResult struct {
	Name    string            `json:"name"`
	Status  corehealth.Status `json:"status"`
	Latency string            `json:"latency"`
	Error   string            `json:"error,omitempty"`
}

func renderReport(report corehealth.Report, policy corehealth.DetailPolicy) webReport {
	out := webReport{
		Status: report.Status,
		Probe:  report.Kind,
		Checks: make([]webResult, len(report.Checks)),
	}
	for index, result := range report.Checks {
		out.Checks[index] = webResult{
			Name:    result.Name,
			Status:  result.Status,
			Latency: renderLatency(result.Latency),
		}
		if policy == corehealth.DetailAlways && result.Error != nil {
			out.Checks[index].Error = result.Error.Error()
		}
	}
	return out
}

func renderLatency(duration time.Duration) string {
	return duration.Round(time.Microsecond).String()
}
