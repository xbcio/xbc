package gracefulshutdown

import (
	"context"
	"net/http"

	"github.com/xbcio/xbc/transport/web"
)

// RegisterRoutes contributes an operator endpoint when explicitly enabled.
// Access control is delegated to the application's authentication policy.
func (p *Plugin) RegisterRoutes(router *web.Router) {
	cfg := p.endpoint
	if !cfg.enabled {
		return
	}
	router.POST(cfg.path, web.Handle(p.handleShutdown)).
		Name("management.shutdown")
}

func (p *Plugin) handleShutdown(_ context.Context, c *web.Ctx) error {
	cfg := p.endpoint
	if !cfg.enabled {
		c.SetHeader("Cache-Control", "no-store")
		web.AbortProblem(c.Gin(), web.NewProblem(http.StatusNotFound, "not_found"))
		return nil
	}

	if p.controller == nil || !p.controller.Request("http operator request") {
		c.SetHeader("Cache-Control", "no-store")
		web.AbortProblem(c.Gin(), web.NewProblem(http.StatusConflict, "shutdown_already_requested"))
		return nil
	}
	c.SetHeader("Cache-Control", "no-store")
	c.JSON(http.StatusAccepted, map[string]string{"status": "shutting_down"})
	return nil
}
