package securityheaders

import (
	"strings"

	"github.com/gin-gonic/gin"
)

func (p *Plugin) handle(c *gin.Context) {
	state := p.state.Load()
	if state == nil {
		c.Next()
		return
	}
	for _, header := range state.headers {
		if header.value != "" {
			c.Header(header.name, header.value)
		}
	}
	if state.hsts != "" && (!state.hstsOnlyHTTPS || requestIsHTTPS(c, state.hstsTrustForwardedProto)) {
		c.Header("Strict-Transport-Security", state.hsts)
	}
	c.Next()
}

func requestIsHTTPS(c *gin.Context, trustForwardedProto bool) bool {
	if c.Request.TLS != nil {
		return true
	}
	return trustForwardedProto && strings.EqualFold(strings.TrimSpace(c.GetHeader("X-Forwarded-Proto")), "https")
}
