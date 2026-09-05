package jwt

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

// Handler implements web.Middleware.
func (p *Plugin) Handler() gin.HandlerFunc { return p.authenticate }

// Order implements web.Middleware. JWT declares no ordering constraints of
// its own; dependents that must run after authentication reference this
// Definition's Key directly (see web.Prefer and web.Require).
func (p *Plugin) Order() web.Order {
	return web.Order{Phase: web.PhaseAuth}
}

func (p *Plugin) authenticate(c *gin.Context) {
	runtime := p.compiled

	// CurrentRoute reads the request's entry from Web's frozen route index.
	// This must remain request-time work: the middleware is built before
	// application routes are registered, so loading a whitelist there would
	// permanently cache an empty table.
	route, found := web.CurrentRoute(c)
	if found && route.Auth.IsPublic() {
		c.Next()
		return
	}
	if runtime.isExcluded(c, route, found) {
		c.Next()
		return
	}

	raw, ok := bearerToken(c.GetHeader(runtime.header), runtime.scheme)
	if !ok {
		unauthorized(c, runtime.scheme)
		return
	}
	claims, err := runtime.verify(raw)
	if err != nil {
		// Deliberately do not return, log, or serialize raw or err. Signature,
		// expiry, issuer, and parser details are all authentication oracles.
		unauthorized(c, runtime.scheme)
		return
	}
	c.Set(claimsContextKey, claims)
	if subject, err := claims.GetSubject(); err == nil && subject != "" {
		web.SetPrincipal(c, web.Principal{
			Subject:    subject,
			AuthMethod: "jwt",
			Attributes: map[string]any(claims),
		})
	}
	c.Next()
}

func bearerToken(value, scheme string) (string, bool) {
	fields := strings.Fields(value)
	if len(fields) != 2 || !strings.EqualFold(fields[0], scheme) || fields[1] == "" {
		return "", false
	}
	return fields[1], true
}

func (runtime *compiledConfig) isExcluded(c *gin.Context, route web.RouteInfo, found bool) bool {
	if len(runtime.exclude) == 0 {
		return false
	}

	paths := make([]string, 0, 3)
	if found && route.Path != "" {
		paths = append(paths, route.Path)
	}
	if fullPath := c.FullPath(); fullPath != "" {
		paths = append(paths, fullPath)
	}
	if c.Request != nil && c.Request.URL != nil && c.Request.URL.Path != "" {
		paths = append(paths, c.Request.URL.Path)
	}

	method := ""
	if c.Request != nil {
		method = strings.ToUpper(c.Request.Method)
	}
	for _, rule := range runtime.exclude {
		if rule.method != "" && rule.method != method {
			continue
		}
		for _, candidate := range paths {
			if candidate == rule.path {
				return true
			}
		}
	}
	return false
}

func unauthorized(c *gin.Context, scheme string) {
	if scheme != "" {
		c.Header("WWW-Authenticate", scheme)
	}
	web.AbortProblem(c, web.NewProblem(http.StatusUnauthorized, "unauthorized"))
}
