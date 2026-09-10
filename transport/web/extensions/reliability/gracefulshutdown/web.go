package gracefulshutdown

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

// RegisterRoutes contributes an operator endpoint when explicitly enabled.
// Access control is delegated to the application's authentication policy.
func (p *Plugin) RegisterRoutes(router *web.Router) {
	cfg := p.endpoint
	if !cfg.enabled {
		return
	}
	router.POST(cfg.path, p.handleShutdown).
		Name("management.shutdown")
}

func (p *Plugin) handleShutdown(c *gin.Context) {
	cfg := p.endpoint
	if !cfg.enabled {
		c.Header("Cache-Control", "no-store")
		web.AbortProblem(c, web.NewProblem(http.StatusNotFound, "not_found"))
		return
	}

	if p.controller == nil || !p.controller.Request("http operator request") {
		c.Header("Cache-Control", "no-store")
		web.AbortProblem(c, web.NewProblem(http.StatusConflict, "shutdown_already_requested"))
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusAccepted, gin.H{"status": "shutting_down"})
}
