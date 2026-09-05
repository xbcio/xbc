package metrics

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/accesslog"
)

// Handler returns the Gin request-instrumentation middleware.
func (p *Plugin) Handler() gin.HandlerFunc { return p.handleRequest }

// Order places metrics in the observation phase before optional access
// logging. Tracing owns its optional edge into metrics, avoiding a reciprocal
// package dependency.
func (*Plugin) Order() web.Order {
	return web.Order{
		Phase:  web.PhaseObserve,
		Before: []web.OrderRef{web.Prefer(accesslog.Key)},
	}
}

func (p *Plugin) handleRequest(c *gin.Context) {
	state := p.state.Load()
	if state == nil {
		c.Next()
		return
	}

	method := boundedMethod(c.Request.Method)
	route := "unmatched"
	if info, ok := web.CurrentRoute(c); ok && info.Path != "" {
		route = info.Path
	}

	state.inflight.WithLabelValues(method, route).Inc()
	started := time.Now()
	panicked := true
	defer func() {
		state.inflight.WithLabelValues(method, route).Dec()
		status := c.Writer.Status()
		if panicked && !c.Writer.Written() {
			status = http.StatusInternalServerError
		}
		statusClass := boundedStatusClass(status)
		state.requests.WithLabelValues(method, route, statusClass).Inc()
		state.duration.WithLabelValues(method, route, statusClass).Observe(time.Since(started).Seconds())
	}()
	c.Next()
	panicked = false
}

func boundedMethod(method string) string {
	switch method = strings.ToUpper(strings.TrimSpace(method)); method {
	case http.MethodConnect, http.MethodDelete, http.MethodGet, http.MethodHead,
		http.MethodOptions, http.MethodPatch, http.MethodPost, http.MethodPut, http.MethodTrace:
		return method
	default:
		return "OTHER"
	}
}

func boundedStatusClass(status int) string {
	switch status / 100 {
	case 1:
		return "1xx"
	case 2:
		return "2xx"
	case 3:
		return "3xx"
	case 4:
		return "4xx"
	case 5:
		return "5xx"
	default:
		return "unknown"
	}
}
