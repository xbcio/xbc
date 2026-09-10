package web

import (
	"fmt"

	"github.com/xbcio/xbc/authentication"
)

// policyTier names which precedence level decided a route's effective policy.
// It exists for the startup report: an operator diagnosing "why is this route
// public" needs to know whether an application rule, the route's own
// declaration, or the global fallback made that call.
type policyTier uint8

const (
	// tierApplicationRule is an explicit rule from web.security.policies. It
	// outranks everything, which is what makes the application the final arbiter.
	tierApplicationRule policyTier = iota + 1
	// tierRoute is the route's own .Auth() declaration.
	tierRoute
	// tierDefault is the global fallback. It never overrides a route
	// declaration; it only fills in routes nothing else covered.
	tierDefault
)

func (t policyTier) String() string {
	switch t {
	case tierApplicationRule:
		return "application-rule"
	case tierRoute:
		return "route"
	case tierDefault:
		return "default"
	default:
		return "invalid"
	}
}

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
