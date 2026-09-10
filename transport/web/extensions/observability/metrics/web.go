package metrics

import (
	"net/http"

	"github.com/gin-gonic/gin"
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

func (p *Plugin) handleMetrics(c *gin.Context) {
	state := p.state.Load()
	if state == nil || !state.config.endpoint.enabled {
		web.AbortProblem(c, web.NewProblem(http.StatusNotFound, "not_found"))
		return
	}
	c.Header("Cache-Control", "no-store")
	promhttp.HandlerFor(state.registry.inner, *state.handler).ServeHTTP(c.Writer, c.Request)
}
