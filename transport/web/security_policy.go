package web

import (
	"fmt"

	"github.com/xbcio/xbc/extensions/authentication"
)

// PolicySource names which precedence level decided a route's effective
// policy. It exists for the startup report: an operator diagnosing "why is
// this route public" needs to know whether an application rule, the route's own
// declaration, or the global fallback made that call. PolicyResolver.Resolve
// reports the same value, so reading the report and querying the policy never
// name the tier differently.
type PolicySource uint8

const (
	// PolicySourceRule is an explicit rule from web.security.policies. It
	// outranks everything, which is what makes the application the final arbiter.
	PolicySourceRule PolicySource = iota + 1
	// PolicySourceRoute is the route's own .Auth() declaration.
	PolicySourceRoute
	// PolicySourceDefault is the global fallback. It never overrides a route
	// declaration; it only fills in routes nothing else covered.
	PolicySourceDefault
)

func (s PolicySource) String() string {
	switch s {
	case PolicySourceRule:
		return "application-rule"
	case PolicySourceRoute:
		return "route"
	case PolicySourceDefault:
		return "default"
	default:
		return "invalid"
	}
}

// policyTier and the tier* constants are the unexported spelling the runtime
// resolution code uses for the same values PolicySource reports.
type policyTier = PolicySource

const (
	tierApplicationRule = PolicySourceRule
	tierRoute           = PolicySourceRoute
	tierDefault         = PolicySourceDefault
)

// effectivePolicy is one frozen route's resolved authentication decision.
//
// ruleIndex is meaningful only when tier is tierApplicationRule; it is -1
// otherwise so the startup report never prints a rule number for a decision no
// rule made.
type effectivePolicy struct {
	permit    bool
	selection authentication.Selection
	tier      policyTier
	ruleIndex int
}

// compiledRule pairs a parsed matcher with the rule it came from.
type compiledRule struct {
	matcher matcher
	rule    PolicyRule
	index   int
}

// policySet is the immutable, parsed form of SecurityConfig. It is built once
// during Start and consulted only through compile, so the linear rule scan is
// paid at startup rather than per request.
type policySet struct {
	rules           []compiledRule
	defaultDecision SecurityDefault
}

// newPolicySet parses every rule up front. Validate has already rejected
// malformed expressions, so a parse failure here is a programming error in the
// call order rather than a user configuration mistake.
func newPolicySet(cfg SecurityConfig) (*policySet, error) {
	set := &policySet{
		rules:           make([]compiledRule, 0, len(cfg.Policies)),
		defaultDecision: cfg.Default,
	}
	for i, rule := range cfg.Policies {
		parsed, err := parseMatch(rule.Match)
		if err != nil {
			return nil, fmt.Errorf("web: security policy %d: %w", i, err)
		}
		set.rules = append(set.rules, compiledRule{matcher: parsed, rule: rule, index: i})
	}
	return set, nil
}

// resolve applies the three precedence tiers to one frozen route.
func (s *policySet) resolve(route RouteInfo) effectivePolicy {
	for _, candidate := range s.rules {
		if !candidate.matcher.matches(route.Method, route.Path) {
			continue
		}
		if candidate.rule.Permit {
			return effectivePolicy{
				permit:    true,
				tier:      tierApplicationRule,
				ruleIndex: candidate.index,
			}
		}
		return effectivePolicy{
			selection: authentication.SelectSchemes(candidate.rule.Authenticate...),
			tier:      tierApplicationRule,
			ruleIndex: candidate.index,
		}
	}

	if route.Auth != nil {
		if route.Auth.IsPublic() {
			return effectivePolicy{permit: true, tier: tierRoute, ruleIndex: -1}
		}
		if schemes := route.Auth.Schemes(); len(schemes) > 0 {
			return effectivePolicy{
				selection: authentication.SelectSchemes(schemes...),
				tier:      tierRoute,
				ruleIndex: -1,
			}
		}
	}

	return effectivePolicy{
		permit:    s.defaultDecision == SecurityPermit,
		selection: authentication.DefaultSelection(),
		tier:      tierDefault,
		ruleIndex: -1,
	}
}

// compile precomputes the effective policy for every frozen route so request
// time resolution is a single map lookup. Rule matching is linear, and paying
// that per request would make policy cost scale with configuration size.
func (s *policySet) compile(routes []RouteInfo) map[string]effectivePolicy {
	compiled := make(map[string]effectivePolicy, len(routes))
	for _, route := range routes {
		compiled[routeKey(route.Method, route.Path)] = s.resolve(route)
	}
	return compiled
}

// PolicyOutcome classifies the effective decision PolicyResolver.Resolve
// returns.
type PolicyOutcome uint8

const (
	// PolicyPermit means the route is served without authentication: either an
	// application rule or the route's own declaration resolved to public.
	PolicyPermit PolicyOutcome = iota + 1
	// PolicyAuthenticate means the route requires a credential for one of the
	// schemes in PolicyDecision.Schemes, selected by an application rule or by
	// the route's own declaration.
	PolicyAuthenticate
	// PolicyDeny means neither an application rule nor a route declaration
	// covers the route and web.security.default is "deny". The request must
	// authenticate through the authentication manager's default scheme set,
	// which PolicyDecision.Schemes reports; it does not mean the request is
	// rejected without consulting an authenticator.
	PolicyDeny
	// PolicyNotApplicable means web.security does not govern the route at all:
	// it is registered on the management plane, whose chain runs no
	// authentication middleware. PolicyDecision.Schemes is empty and Source is
	// unused for this outcome.
	PolicyNotApplicable
)

func (o PolicyOutcome) String() string {
	switch o {
	case PolicyPermit:
		return "permit"
	case PolicyAuthenticate:
		return "authenticate"
	case PolicyDeny:
		return "deny"
	case PolicyNotApplicable:
		return "not-applicable"
	default:
		return "invalid"
	}
}

// PolicyDecision is the resolved authentication policy for one frozen route.
//
// It carries exactly what the runtime decided, plus the provenance needed to
// explain the decision: which precedence tier made it and, when a rule made it,
// that rule's declaration index and original match expression. Values are
// snapshots; mutating the returned slice never affects the server.
type PolicyDecision struct {
	// Outcome is the effective decision.
	Outcome PolicyOutcome
	// Schemes lists the authentication schemes a request is checked against, in
	// arbitration order. It is empty for PolicyPermit and PolicyNotApplicable,
	// and for PolicyDeny it is the manager's default scheme set.
	Schemes []authentication.Scheme
	// Source names the tier that decided: an application rule, the route's own
	// declaration, or the global default.
	Source PolicySource
	// RuleIndex is the zero-based position in web.security.policies of the rule
	// that decided, or -1 when no rule decided.
	RuleIndex int
	// RuleMatch is that rule's original match expression, or empty when no rule
	// decided.
	RuleMatch string
}

// PolicyResolver is a read-only view of an application's compiled
// authentication policy. The Server constructs it after the route table has
// been validated and hands it to every RouteCatalogListener beside the frozen
// catalog; it is the same policySet the request path enforces, so it answers
// with the same precedence implementation rather than a reimplementation.
//
// A resolver obtained from RoutesReady is immutable and safe for concurrent
// use. The zero value is fail-closed: it resolves every serving route as the
// strict default ("deny", no default schemes), which is what a caller holding
// a PolicyResolver that was never populated observes.
type PolicyResolver struct {
	policies       *policySet
	defaultSchemes []authentication.Scheme
}

// Resolve returns the effective authentication decision for route, using the
// same precedence implementation the request path uses -- the application rule
// list first, then the route's own .Auth() declaration, then the global
// default. Passing a RouteInfo from the frozen catalog (or one equal to it)
// yields what that route's requests are held to.
//
// A route registered on the management plane is resolved to
// PolicyNotApplicable: that plane runs no authentication middleware, so
// web.security neither decides nor reports a policy for it.
func (r PolicyResolver) Resolve(route RouteInfo) PolicyDecision {
	if route.Management {
		return PolicyDecision{Outcome: PolicyNotApplicable, RuleIndex: -1}
	}
	policy := effectivePolicy{
		selection: authentication.DefaultSelection(),
		tier:      tierDefault,
		ruleIndex: -1,
	}
	if r.policies != nil {
		policy = r.policies.resolve(route)
	}

	decision := PolicyDecision{Source: policy.tier, RuleIndex: policy.ruleIndex}
	if policy.tier == tierApplicationRule && r.policies != nil &&
		policy.ruleIndex >= 0 && policy.ruleIndex < len(r.policies.rules) {
		decision.RuleMatch = r.policies.rules[policy.ruleIndex].rule.Match
	}
	switch {
	case policy.permit:
		decision.Outcome = PolicyPermit
	case policy.selection.UsesDefault():
		// A non-permit default decision is deny-by-default: the manager still
		// attempts its default scheme set, so the schemes are reported rather
		// than leaving the caller to guess them.
		decision.Outcome = PolicyDeny
		decision.Schemes = append([]authentication.Scheme(nil), r.defaultSchemes...)
	default:
		decision.Outcome = PolicyAuthenticate
		decision.Schemes = policy.selection.Schemes()
	}
	return decision
}

// policyRuleCoverage records how many frozen serving routes one application
// rule matches. Coverage is computed once during startup: it feeds both the
// mandatory-rule check and the startup report, so the line an operator reads
// and the check that let the process boot can never disagree.
type policyRuleCoverage struct {
	rule  PolicyRule
	index int
	hits  int
}

// coverage counts the frozen serving routes each rule matches. Matching is
// done against RouteInfo rows exactly as runtime resolution does it, so a
// mounted subtree counts as one hit at its prefix row -- a rule naming the
// prefix covers the row, which is what makes the subtree reachable.
func (s *policySet) coverage(routes []RouteInfo) []policyRuleCoverage {
	coverages := make([]policyRuleCoverage, len(s.rules))
	for i, candidate := range s.rules {
		hits := 0
		for _, route := range routes {
			if candidate.matcher.matches(route.Method, route.Path) {
				hits++
			}
		}
		coverages[i] = policyRuleCoverage{rule: candidate.rule, index: candidate.index, hits: hits}
	}
	return coverages
}

// validateCoverage rejects a mandatory rule that matches no frozen serving
// route. Rules are the application's final say over endpoint access, so a rule
// that matches nothing is almost always a typo in Match -- and its intended
// endpoints keep whatever policy the lower tiers gave them. Failing startup
// makes that mistake visible instead of letting it serve.
//
// Management-plane rows are excluded, exactly as they are from policy
// compilation: web.security does not govern that plane, so a rule that only
// names a management route has no effect to validate. A rule may opt out with
// Optional when it targets routes compiled into another build.
func (s *policySet) validateCoverage(coverages []policyRuleCoverage) error {
	for _, coverage := range coverages {
		if coverage.hits > 0 || coverage.rule.Optional {
			continue
		}
		return fmt.Errorf(
			"web: security policy %d (%q) matches no route in the frozen serving route table; "+
				"fix the match expression, or set optional: true when the rule only targets routes "+
				"compiled into another build",
			coverage.index, coverage.rule.Match,
		)
	}
	return nil
}

// validateReachability rejects a rule that an earlier, broader rule already
// decides for every route it could name. Because rules are first-match-wins and
// both the rule list and the route table are frozen at startup, an unreachable
// rule is statically detectable -- and it is almost always a mistake in rule
// order rather than an intentionally dead rule.
func (s *policySet) validateReachability() error {
	for i, candidate := range s.rules {
		for j := 0; j < i; j++ {
			earlier := s.rules[j]
			if !earlier.matcher.covers(candidate.matcher) {
				continue
			}
			return fmt.Errorf(
				"web: security policy %d (%q) is unreachable: policy %d (%q) already matches every route it names; "+
					"write the more specific rule first",
				i, candidate.rule.Match, j, earlier.rule.Match,
			)
		}
	}
	return nil
}

// referencedSchemes returns every scheme named by an authenticate rule, in
// first-appearance order.
func (s *policySet) referencedSchemes() []authentication.Scheme {
	seen := make(map[authentication.Scheme]struct{})
	var schemes []authentication.Scheme
	for _, candidate := range s.rules {
		for _, scheme := range candidate.rule.Authenticate {
			if _, duplicate := seen[scheme]; duplicate {
				continue
			}
			seen[scheme] = struct{}{}
			schemes = append(schemes, scheme)
		}
	}
	return schemes
}

// validateSchemeCoverage rejects a registered authenticator that no effective
// policy can ever select. Selecting a plugin and then never naming it is a
// half-finished migration or a typo, and failing startup is cheaper than
// discovering it when the credential it was meant to accept is rejected in
// production.
//
// A scheme is reachable when either tier names it:
//
//   - a tier-1 rule authenticates with it (referencedSchemes), or
//   - a serving route the rules do not cover resolves to an explicit selection
//     naming it. Route-level .Auth(Accepts(...)) is tier 2 and never enters the
//     rule list, so scanning rules alone rejected the legitimate
//     "default: permit plus a route declaration" composition.
//
// A route covered by a tier-1 rule contributes nothing here: its compiled
// selection is the rule's own selection, which referencedSchemes already
// collects, or the rule is a permit rule, which selects no scheme. The !ok
// guard is defensive: compiled is built from this same route slice.
//
// Under default deny the check is skipped: uncovered routes resolve through the
// manager's restrictive default selection. This is sound only because the
// server builds the Manager with DefaultSchemes set to every registered scheme,
// so an unnamed scheme is still reachable. Narrowing DefaultSchemes to a subset
// would turn this skip into a hole and this check must then compare against the
// union of referenced schemes and the manager's actual defaults.
func (s *policySet) validateSchemeCoverage(
	registered []authentication.Scheme,
	routes []RouteInfo,
	compiled map[string]effectivePolicy,
) error {
	if s.defaultDecision == SecurityDeny {
		return nil
	}
	referenced := make(map[authentication.Scheme]struct{}, len(s.rules))
	for _, scheme := range s.referencedSchemes() {
		referenced[scheme] = struct{}{}
	}
	for _, route := range routes {
		policy, ok := compiled[routeKey(route.Method, route.Path)]
		if !ok || policy.permit {
			continue
		}
		for _, scheme := range policy.selection.Schemes() {
			referenced[scheme] = struct{}{}
		}
	}
	for _, scheme := range registered {
		if _, ok := referenced[scheme]; !ok {
			return fmt.Errorf(
				"web: authentication scheme %q is registered but no security policy or route "+
					"authentication declaration references it; name it in a policy or a route "+
					".Auth() declaration, or remove the plugin",
				scheme,
			)
		}
	}
	return nil
}
