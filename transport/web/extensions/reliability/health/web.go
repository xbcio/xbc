package health

import (
	"context"
	"net/http"
	"time"

	corehealth "github.com/xbcio/xbc/extensions/reliability/health"
	transportweb "github.com/xbcio/xbc/transport/web"
)

// RegisterRoutes implements web.RouteContributor. Probe endpoints explicitly
// bypass authentication so operational health never depends on credentials.
func (p *Plugin) RegisterRoutes(router *transportweb.Router) {
	router.GET(p.cfg.LivenessPath, p.probeHandler(corehealth.Liveness)).Auth(transportweb.Public())
	router.GET(p.cfg.ReadinessPath, p.probeHandler(corehealth.Readiness)).Auth(transportweb.Public())
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
