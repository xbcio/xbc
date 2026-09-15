package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/plugin"
)

// policyDecision records how one frozen route resolved. It exists for the
// startup report: an operator must be able to see that an application rule
// overrode a route's own .Auth() declaration without reading plugin source.
type policyDecision struct {
	route  RouteInfo
	policy effectivePolicy
}

// authenticationMiddleware is the single place a request's authentication
// policy is decided and a Principal is published. It replaces the per-plugin
// exemption checks that previously asked "does this request need auth" in four
// different ways.
//
// It is both a Middleware and a RouteCatalogListener: policy compilation and
// the three startup validations need the frozen route table, which only exists
// after freeze, while request handling needs the compiled table.
type authenticationMiddleware struct {
	manager    *authentication.Manager
	policies   *policySet
	extractors map[authentication.Scheme]CredentialExtractor

	compiled map[string]effectivePolicy
	resolved []policyDecision
	// permitAll records that the whole service runs fail-open. Nothing on the
	// request path reads it: it exists for the startup report, which must print
	// a prominent warning when "default: permit" is in effect, alongside
	// publicRoutes and decisions.
	permitAll bool
}

var (
	_ Middleware           = (*authenticationMiddleware)(nil)
	_ RouteCatalogListener = (*authenticationMiddleware)(nil)
)

// authenticationExemptContextKey marks that this request's authentication
// policy resolved to permit inside the authentication middleware itself.
// AuthenticationExempt reads it so authorization middleware asks the
// framework what exemption actually applied to this specific request,
// instead of re-deriving the answer from a route's own .Auth() declaration --
// a tier-2 signal a tier-1 web.security rule may have overridden.
const authenticationExemptContextKey = "xbc/transport/web.authenticationExempt"

// markAuthenticationExempt records that this request's authentication policy
// resolved to permit. It is called only from the same branch that skips
// calling the authentication manager, so the two facts never disagree.
func markAuthenticationExempt(c *Ctx) {
	c.Set(authenticationExemptContextKey, true)
}

// AuthenticationExempt reports whether the authentication middleware resolved
// this request to permit. Authorization middleware must ask this instead of
// RouteInfo.Auth.IsPublic(): a route's own declaration is only a tier-2
// signal, and an application rule in web.security may have tightened or
// loosened it for this specific request -- the application is the final
// arbiter. A request that never reached the authentication middleware (for
// example a test that wires up authorization middleware standalone) reports
// false: fail closed, not open.
func AuthenticationExempt(c *Ctx) bool {
	if c == nil {
		return false
	}
	value, ok := c.Get(authenticationExemptContextKey)
	if !ok {
		return false
	}
	exempt, ok := value.(bool)
	return ok && exempt
}

// authenticationIdentity is the producer identity the Server attributes its
// built-in authentication middleware to. It is not a selectable plugin -- see
// newAuthenticationMiddleware -- but it participates in the middleware ordering
// graph under the same key, so Require(AuthenticationMiddlewareKey) resolves
// and RequiresPrincipal consumers get a real predecessor edge.
var authenticationIdentity = plugin.Identity{Plugin: AuthenticationMiddlewareKey}

// newAuthenticationMiddleware assembles the framework's single authentication
// middleware from the Web configuration section and the collected
// authentication contributions.
//
// This is deliberately not a plugin Definition. The policy it enforces lives at
// web.security, and a configuration section has exactly one owning plugin, so a
// second Definition claiming ConfigPath would fail universe construction. The
// Server owns the web section and therefore owns this middleware; it installs
// it under authenticationIdentity rather than through the plugin catalog.
func newAuthenticationMiddleware(
	security SecurityConfig,
	authenticators []plugin.Entry[authentication.Authenticator],
	extractorEntries []plugin.Entry[CredentialExtractor],
) (*authenticationMiddleware, error) {
	policies, err := newPolicySet(security.normalize())
	if err != nil {
		return nil, err
	}
	extractors, err := newExtractorIndex(extractorEntries)
	if err != nil {
		return nil, err
	}

	middleware := &authenticationMiddleware{policies: policies, extractors: extractors}
	if len(authenticators) == 0 {
		// NewManager rejects an empty authenticator set. A service whose
		// endpoints are all permitted is still legitimate, so leave manager nil
		// and let RoutesReady fail if any route actually needs one.
		return middleware, nil
	}

	ordered := make([]authentication.Authenticator, len(authenticators))
	schemes := make([]authentication.Scheme, len(authenticators))
	for i, entry := range authenticators {
		ordered[i] = entry.Value
		schemes[i] = entry.Value.Scheme()
	}
	// DefaultSchemes is every registered scheme on purpose. Under
	// "default: deny" a route that no rule covers resolves through the manager's
	// default selection, and validateSchemeCoverage skips its check for exactly
	// that reason; narrowing this set would turn that skip into a hole where a
	// registered scheme is unreachable.
	manager, err := authentication.NewManager(authentication.ManagerOptions{
		Authenticators: ordered,
		DefaultSchemes: schemes,
	})
	if err != nil {
		return nil, err
	}
	middleware.manager = manager
	return middleware, nil
}

// Order places authentication in PhaseAuth. The Server additionally pins this
// middleware outermost within that phase, so every authorization middleware
// sharing the phase observes a published Principal.
func (*authenticationMiddleware) Order() Order { return Order{Phase: PhaseAuth} }

// RoutesReady compiles the per-route policy table and runs every startup
// validation. Returning an error here fails startup before traffic opens,
// which is the whole point: a policy mistake must never degrade into a
// silently permissive service.
func (m *authenticationMiddleware) RoutesReady(catalog RouteCatalog) error {
	routes := catalog.All()

	if err := m.policies.validateReachability(); err != nil {
		return err
	}
	if m.manager != nil {
		if err := m.policies.validateSchemeCoverage(m.manager.Schemes()); err != nil {
			return err
		}
	}

	compiled := m.policies.compile(routes)
	decisions := make([]policyDecision, 0, len(routes))
	for _, route := range routes {
		policy := compiled[routeKey(route.Method, route.Path)]
		decisions = append(decisions, policyDecision{route: route, policy: policy})
		if policy.permit {
			continue
		}
		if m.manager == nil {
			return fmt.Errorf(
				"xbc: web route %s %s requires authentication but no authenticator is registered",
				route.Method, route.Path,
			)
		}
		// First production caller of ValidateSelection: every selection is
		// checked against the registered schemes before traffic opens.
		if err := m.manager.ValidateSelection(policy.selection); err != nil {
			return fmt.Errorf("xbc: web route %s %s: %w", route.Method, route.Path, err)
		}
	}

	m.compiled = compiled
	m.resolved = decisions
	m.permitAll = m.policies.defaultDecision == SecurityPermit
	return nil
}

// Handler resolves policy with a single map lookup. The linear rule scan was
// already paid once during RoutesReady.
func (m *authenticationMiddleware) Handler() Handler {
	return func(_ context.Context, c *Ctx) error {
		route, matched := CurrentRoute(c)
		if !matched {
			// 404 and 405 requests reach here because Server splices the global
			// chain into the NoRoute and NoMethod chains. Turning them into 401
			// would pollute the response semantics and leak which paths exist,
			// and there is no handler behind them to protect.
			c.Next()
			return nil
		}

		policy, known := m.compiled[routeKey(route.Method, route.Path)]
		if !known {
			// The compiled table is built from the same frozen route table that
			// recorded this route, so a miss means the table is stale or was
			// never built at all. Releasing the request would be precisely the
			// silently permissive service RoutesReady exists to prevent, so an
			// internal inconsistency fails closed.
			abortAuthenticationFailure(c, fmt.Errorf(
				"xbc: web route %s %s has no compiled authentication policy",
				route.Method, route.Path,
			))
			return nil
		}
		if policy.permit {
			markAuthenticationExempt(c)
			c.Next()
			return nil
		}

		result, err := m.manager.Authenticate(
			c.Request().Context(),
			policy.selection,
			requestCredentialSource{ctx: c, extractors: m.extractors},
		)
		if err != nil {
			// An operational failure may carry an unsafe cause, so it is logged
			// server-side and never reflected in the response.
			abortAuthenticationFailure(c, err)
			return nil
		}

		if result.Authenticated() {
			principal, ok := result.Principal()
			if !ok {
				abortAuthenticationFailure(c, errors.New("xbc: authenticated result carried no principal"))
				return nil
			}
			typed, ok := principal.(Principal)
			if !ok || !SetPrincipal(c, typed) {
				abortAuthenticationFailure(c, fmt.Errorf(
					"xbc: authenticator returned %T, want web.Principal", principal,
				))
				return nil
			}
			c.Next()
			return nil
		}

		writeAuthenticationRejection(c, result)
		return nil
	}
}

// abortAuthenticationFailure answers an internal authentication failure with a
// fixed generic problem and records the cause server-side.
//
// It deliberately does not route err through AbortError and the OnError mapper
// chain. A failure raised here routinely carries credential material or
// infrastructure detail, and an application-registered mapper is free to turn
// an error it recognizes into a revealing response. The response is therefore
// chosen here and is identical for every cause, so it cannot be used as an
// oracle for which internal step failed.
//
// Logging must happen here too, not at the error boundary: AbortProblem writes
// the response before this returns, so err is never handed back to Handler's
// return path -- every caller returns nil once the response has been chosen
// here. Routing it there instead would not help either: the error boundary
// reached with a response already committed can no longer map or render that
// error, and an engine adapter draining an engine-native error accumulator
// skips an already-written request outright. Either way the cause is lost
// unless it is recorded here.
//
// Only err.Error() reaches the log, and that is deliberate. An
// authentication.OperationalError renders the failed operation and scheme while
// omitting its wrapped cause, precisely because an extractor's cause may quote
// the credential it rejected. Do not unwrap here to "improve" the diagnostic:
// that would copy credential material into the log, the leak the eliding
// Error() exists to prevent.
func abortAuthenticationFailure(c *Ctx, err error) {
	problem := NewProblem(http.StatusInternalServerError, "authentication_failed")
	if err != nil {
		resolverFor(c).log(err, problem.Status, c, false)
	}
	AbortProblem(c, problem)
}

// writeAuthenticationRejection renders 401 with one WWW-Authenticate header per
// challenge. RFC 7235 permits repeating the header, and a challenge's own
// parameters may contain commas, so comma-joining would require escaping for no
// benefit. The headers are added before AbortProblem because WriteProblem only
// deletes payload-describing headers, never authentication ones.
func writeAuthenticationRejection(c *Ctx, result authentication.Result) {
	for _, challenge := range result.Challenges() {
		c.Writer().Header().Add("WWW-Authenticate", string(challenge))
	}
	problem := NewProblem(http.StatusUnauthorized, "unauthenticated")
	if reason, ok := result.Reason(); ok {
		problem.Detail = string(reason)
	}
	AbortProblem(c, problem)
}

// publicRoutes lists every frozen route that resolves to permit. The startup
// report prints it so an operator does not have to read plugin source to learn
// which endpoints are open.
func (m *authenticationMiddleware) publicRoutes() []RouteInfo {
	var public []RouteInfo
	for _, decision := range m.resolved {
		if decision.policy.permit {
			public = append(public, decision.route)
		}
	}
	return public
}

// decisions returns every resolved route decision, including the tier that made
// it, for the startup report.
func (m *authenticationMiddleware) decisions() []policyDecision {
	return append([]policyDecision(nil), m.resolved...)
}
