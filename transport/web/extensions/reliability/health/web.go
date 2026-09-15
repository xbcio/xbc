package health

import (
	"context"
	"net/http"
	"time"

	"github.com/xbcio/xbc/plugin"
	transportweb "github.com/xbcio/xbc/transport/web"
)

var _ transportweb.RouteContributor = (*Plugin)(nil)

func routeContracts() plugin.ContractSet[*Plugin] {
	return plugin.Contracts(
		plugin.ExportAs[transportweb.RouteContributor](func(value *Plugin) transportweb.RouteContributor { return value }),
	)
}

// RegisterRoutes implements web.RouteContributor. Probe endpoints explicitly
// bypass authentication so operational health never depends on credentials.
func (p *Plugin) RegisterRoutes(router *transportweb.Router) {
	router.GET(p.cfg.LivenessPath, p.probeHandler(Liveness)).Auth(transportweb.Public())
	router.GET(p.cfg.ReadinessPath, p.probeHandler(Readiness)).Auth(transportweb.Public())
}

func (p *Plugin) probeHandler(kind Kind) transportweb.Handler {
	return func(ctx context.Context, c *transportweb.Ctx) error {
		report := p.Check(ctx, kind)
		statusCode := http.StatusOK
		if !report.Healthy() {
			statusCode = http.StatusServiceUnavailable
		}
		c.JSON(statusCode, renderReport(report, p.cfg.DetailPolicy))
		return nil
	}
}

type webReport struct {
	Status Status      `json:"status"`
	Probe  Kind        `json:"probe"`
	Checks []webResult `json:"checks"`
}

type webResult struct {
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Latency string `json:"latency"`
	Error   string `json:"error,omitempty"`
}

func renderReport(report Report, policy DetailPolicy) webReport {
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
		if policy == DetailAlways && result.Error != nil {
			out.Checks[index].Error = result.Error.Error()
		}
	}
	return out
}

func renderLatency(duration time.Duration) string {
	return duration.Round(time.Microsecond).String()
}
