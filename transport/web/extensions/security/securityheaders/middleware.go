package securityheaders

import (
	"context"
	"strings"

	"github.com/xbcio/xbc/transport/web"
)

func (p *Plugin) handle(_ context.Context, c *web.Ctx) error {
	state := p.state.Load()
	if state == nil {
		c.Next()
		return nil
	}
	for _, header := range state.headers {
		if header.value != "" {
			c.SetHeader(header.name, header.value)
		}
	}
	if state.hsts != "" && (!state.hstsOnlyHTTPS || requestIsHTTPS(c, state.hstsTrustForwardedProto)) {
		c.SetHeader("Strict-Transport-Security", state.hsts)
	}
	c.Next()
	return nil
}

func requestIsHTTPS(c *web.Ctx, trustForwardedProto bool) bool {
	if c.Request().TLS != nil {
		return true
	}
	return trustForwardedProto && strings.EqualFold(strings.TrimSpace(c.GetHeader("X-Forwarded-Proto")), "https")
}
