package metrics

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/xbcio/xbc/transport/web"
)

// RegisterRoutes contributes the exposition endpoint only when explicitly
// enabled. Access control is delegated to the application's authentication
// policy.
func (p *Plugin) RegisterRoutes(router *web.Router) {
	state := p.state.Load()
	if state == nil || !state.config.endpoint.enabled {
		return
	}
	router.GET(state.config.endpoint.path, p.handleMetrics).
		Name("management.metrics")
}

func (p *Plugin) handleMetrics(_ context.Context, c *web.Ctx) error {
	state := p.state.Load()
	if state == nil || !state.config.endpoint.enabled {
		web.AbortProblem(c.Gin(), web.NewProblem(http.StatusNotFound, "not_found"))
		return nil
	}
	c.SetHeader("Cache-Control", "no-store")
	promhttp.HandlerFor(state.registry.inner, *state.handler).ServeHTTP(c.Writer(), c.Request())
	return nil
}
