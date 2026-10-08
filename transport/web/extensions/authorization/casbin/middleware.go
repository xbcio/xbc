package casbin

import (
	"context"
	"net/http"
	"strings"

	casbincore "github.com/xbcio/xbc/extensions/authorization/casbin"
	"github.com/xbcio/xbc/transport/web"
)

// Handler implements web.Middleware.
func (p *Plugin) Handler() web.Handler { return p.authorize }

// Order implements web.Middleware. casbin-http deliberately does not also
// declare authentication.RequiresPrincipal, unlike tenant and idempotency:
// its actual identity dependency is SubjectResolver, not
// authentication.Principal specifically (see WithSubjectResolver and
// TestInjectedResolverDoesNotRequireJWTOrPrincipal), and an injected resolver
// may source a verified subject from somewhere other than Web's built-in
// authentication middleware entirely. This After already orders the
// middleware behind whatever the framework's own authentication middleware
// resolved for this request -- including the no-Principal AuthenticationExempt
// and AcceptedWithoutPrincipal outcomes authorize checks below -- which is the
// whole of what route enforcement needs from authentication ordering; adding
// the marker would additionally claim a coupling to the Principal contract
// that the resolver abstraction exists to avoid.
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
		// unmatched route (404/405) straight through to preserve Web's own
		// 404/405 response (see the comment on tenant's equivalent branch).
		// Rewriting it to 403 here is pre-existing behavior, not introduced by
		// the exemption-model change: flipping it would alter the observable
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

	enforcer, active := state.provider.Enforcer()
	if !active || enforcer == nil {
		// The engine reports itself unavailable after its own Stop, so a
		// middleware that outlives the engine fails closed here instead of
		// enforcing against a torn-down policy source.
		forbidden(c)
		return nil
	}

	subject, ok := state.resolver.ResolveSubject(c)
	subject = strings.TrimSpace(subject)
	if !ok || subject == "" {
		forbidden(c)
		return nil
	}

	var request []any
	switch state.convention {
	case casbincore.ConventionRoutePermission:
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
	case casbincore.ConventionPathMethod:
		object := strings.TrimSpace(route.Path)
		action := strings.ToUpper(strings.TrimSpace(route.Method))
		if object == "" || action == "" {
			forbidden(c)
			return nil
		}
		request = []any{subject, object, action}
	}

	allowed, err := enforcer.Enforce(request...)
	if err != nil {
		state.logger.Error("casbin-http: authorization evaluation failed",
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
