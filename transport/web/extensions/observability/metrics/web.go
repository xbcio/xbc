package metrics

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/xbcio/xbc/transport/web"
)

// RegisterRoutes contributes the exposition endpoint only when plugins.metrics
// explicitly enables it.
//
// The route is registered on the management plane. Scraping what the process is
// doing is not business traffic: it must answer while the admission gate is
// saturated, and where it is reachable from is a property of the listener, not
// of an authentication policy a scraper has no identity to satisfy. With no
// management listener configured -- the default -- this is an ordinary route on
// the serving listener, exactly as it was before the plane existed; see
// web.Router.Management.
func (p *Plugin) RegisterRoutes(router *web.Router) {
	if p == nil || p.core == nil {
		return
	}
	httpConfig := p.core.HTTP()
	if !httpConfig.Enabled {
		return
	}
	router.Management().GET(httpConfig.Path, p.handleMetrics).
		Name("management.metrics")
}

func (p *Plugin) handleMetrics(_ context.Context, c *web.Ctx) error {
	if p == nil || p.core == nil || !p.core.HTTP().Enabled {
		web.AbortProblem(c, web.NewProblem(http.StatusNotFound, "not_found"))
		return nil
	}
	c.SetHeader("Cache-Control", "no-store")
	promhttp.HandlerFor(p.core.Registry(), p.handler).ServeHTTP(c.Writer(), c.Request())
	return nil
}
