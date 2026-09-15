package casbin

import (
	"context"
	"net/http"
	"strings"

	"github.com/xbcio/xbc/transport/web"
)

// Handler implements web.Middleware.
func (p *Plugin) Handler() web.Handler { return p.authorize }

// Order implements web.Middleware.
func (p *Plugin) Order() web.Order {
	return web.Order{
		Phase: web.PhaseAuth,
		After: []web.OrderRef{web.Require(web.AuthenticationMiddlewareKey)},
	}
}

func (p *Plugin) authorize(_ context.Context, c *web.Ctx) error {
	// Ask the framework what exemption actually applied to this request,
	// never the route's own .Auth() declaration: an application rule in
	// web.security is the final arbiter and may have tightened a route that
	// declared itself public.
	if web.AuthenticationExempt(c) {
		c.Next()
		return nil
	}
	route, found := web.CurrentRoute(c)
	if !found {
		// This is a deliberate divergence from tenant, which passes an
		// unmatched route (404/405) straight through to preserve gin's own
		// response (see the comment on tenant's equivalent branch). Rewriting
		// it to 403 here is pre-existing behavior, not introduced by the
		// exemption-model change: flipping it would alter the observable
		// response for every unmatched path in every casbin application, and
		// fail-closed is the safer side to leave standing.
		forbidden(c)
		return nil
	}

	if p.stopped.Load() {
		forbidden(c)
		return nil
	}
	state := p.state

	subject, ok := state.resolver.ResolveSubject(c)
	subject = strings.TrimSpace(subject)
	if !ok || subject == "" {
		forbidden(c)
		return nil
	}

	var request []any
	switch state.cfg.requestConvention {
	case ConventionRoutePermission:
		permission := strings.TrimSpace(route.Perm)
		if permission == "" {
			if state.cfg.missingPermission == MissingPermissionAllow {
				c.Next()
				return nil
			}
			forbidden(c)
			return nil
		}
		request = []any{subject, permission}
	case ConventionPathMethod:
		object := strings.TrimSpace(route.Path)
		action := strings.ToUpper(strings.TrimSpace(route.Method))
		if object == "" || action == "" {
			forbidden(c)
			return nil
		}
		request = []any{subject, object, action}
	default:
		forbidden(c)
		return nil
	}

	allowed, err := state.enforcer.Enforce(request...)
	if err != nil {
		state.logger.Error("casbin: authorization evaluation failed",
			"method", route.Method, "path", route.Path, "error", err)
		forbidden(c)
		return nil
	}
	if !allowed {
		forbidden(c)
		return nil
	}
	c.Next()
	return nil
}

func forbidden(c *web.Ctx) {
	web.AbortProblem(c, web.NewProblem(http.StatusForbidden, "forbidden"))
}
