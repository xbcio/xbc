package session

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

// Handler implements web.Middleware.
func (p *Plugin) Handler() gin.HandlerFunc { return p.authenticate }

// Order implements web.Middleware. Authentication must run before tenant
// resolution and authorization; the references are soft and harmless when
// either sibling plugin is absent.
func (p *Plugin) Order() web.Order {
	return web.Order{
		Phase:  web.PhaseAuth,
		Before: []web.OrderRef{web.Prefer(tenantKey), web.Prefer(casbinKey)},
	}
}

func (p *Plugin) authenticate(c *gin.Context) {
	if route, ok := web.CurrentRoute(c); ok && route.Auth.IsPublic() {
		c.Next()
		return
	}
	if p.manager.closed.Load() || c == nil || c.Request == nil {
		unauthorized(c)
		return
	}
	id, err := extractCookie(c.Request, p.config.name, p.config.idBytes)
	if err != nil {
		p.manager.ClearCookie(c)
		unauthorized(c)
		return
	}
	touchCtx, cancel := context.WithTimeout(c.Request.Context(), p.config.operationTimeout)
	value, found, err := p.manager.store.Touch(touchCtx, id, p.config.idleTTL, p.config.touchInterval)
	cancel()
	if err != nil || !found {
		if err == nil {
			p.manager.ClearCookie(c)
		}
		unauthorized(c)
		return
	}
	if !web.SetPrincipal(c, web.Principal{
		Subject:    value.Subject,
		AuthMethod: "session",
		Attributes: cloneMap(value.Attributes),
	}) {
		unauthorized(c)
		return
	}
	setCurrent(c, value)
	c.Next()
}

func extractCookie(request *http.Request, name string, idBytes int) (string, error) {
	if request == nil {
		return "", ErrInvalidCookie
	}
	cookies := request.CookiesNamed(name)
	if len(cookies) != 1 || cookies[0] == nil || !validID(cookies[0].Value, idBytes) {
		return "", ErrInvalidCookie
	}
	return cookies[0].Value, nil
}

func unauthorized(c *gin.Context) {
	if c == nil {
		return
	}
	web.AbortProblem(c, web.NewProblem(http.StatusUnauthorized, "unauthorized"))
}
