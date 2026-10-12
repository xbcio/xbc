package grpc

import (
	"fmt"
	"strings"

	"github.com/xbcio/xbc/extensions/authentication"
)

// policyTier names which precedence level decided a method's effective policy.
// It exists for the startup report: an operator diagnosing "why is this method
// public" needs to know whether an application rule, the registering plugin's
// declaration, or the global fallback made that call.
type policyTier uint8

const (
	// tierApplicationRule is an explicit rule from grpc.security.rules.
	tierApplicationRule policyTier = iota + 1
	// tierDeclaration is the registering plugin's Auth or MethodAuth call.
	tierDeclaration
	// tierDefault is grpc.security.default. It never overrides a declaration.
	tierDefault
)

func (t policyTier) String() string {
	switch t {
	case tierApplicationRule:
		return "application-rule"
	case tierDeclaration:
		return "declaration"
	case tierDefault:
		return "default"
	default:
		return "invalid"
	}
}

// effectivePolicy is one frozen method's resolved authentication decision.
// ruleIndex is meaningful only for tierApplicationRule and -1 otherwise.
type effectivePolicy struct {
	permit    bool
	selection authentication.Selection
	tier      policyTier
	ruleIndex int
}

type compiledRule struct {
	matcher methodMatcher
	rule    SecurityRule
	index   int
}

// policySet is the parsed form of SecurityConfig, built once during assembly.
type policySet struct {
	rules           []compiledRule
	defaultDecision SecurityDefault
}

func newPolicySet(cfg SecurityConfig) (*policySet, error) {
	set := &policySet{rules: make([]compiledRule, 0, len(cfg.Rules)), defaultDecision: cfg.Default}
	for i, rule := range cfg.Rules {
		parsed, err := parseMethodMatch(rule.Match)
		if err != nil {
			return nil, fmt.Errorf("grpc: security rule %d: %w", i, err)
		}
		set.rules = append(set.rules, compiledRule{matcher: parsed, rule: rule, index: i})
	}
	return set, nil
}

// resolve applies the three precedence tiers to one frozen method.
func (s *policySet) resolve(service string, method methodInfo) effectivePolicy {
	for _, candidate := range s.rules {
		if !candidate.matcher.matches(service, method.name) {
			continue
		}
		if candidate.rule.Permit {
			return effectivePolicy{permit: true, tier: tierApplicationRule, ruleIndex: candidate.index}
		}
		return effectivePolicy{
			selection: authentication.SelectSchemes(candidate.rule.Authenticate...),
			tier:      tierApplicationRule,
			ruleIndex: candidate.index,
		}
	}
	if method.auth != nil {
		if method.auth.IsPublic() {
			return effectivePolicy{permit: true, tier: tierDeclaration, ruleIndex: -1}
		}
		return effectivePolicy{
			selection: authentication.SelectSchemes(method.auth.Schemes()...),
			tier:      tierDeclaration,
			ruleIndex: -1,
		}
	}
	return effectivePolicy{
		permit:    s.defaultDecision == SecurityPermit,
		selection: authentication.DefaultSelection(),
		tier:      tierDefault,
		ruleIndex: -1,
	}
}

// validateReachability rejects a rule an earlier, broader rule already decides
// for every method it could name. Rules are first-match-wins and frozen at
// startup, so an unreachable rule is statically detectable and almost always a
// mistake in rule order.
func (s *policySet) validateReachability() error {
	for i, candidate := range s.rules {
		for j := 0; j < i; j++ {
			earlier := s.rules[j]
			if earlier.matcher.covers(candidate.matcher) {
				return fmt.Errorf(
					"grpc: security rule %d (%q) is unreachable: rule %d (%q) already matches every method it names; write the more specific rule first",
					i, candidate.rule.Match, j, earlier.rule.Match)
			}
		}
	}
	return nil
}

// validateSchemeCoverage rejects a registered authenticator no rule can reach.
// Under default deny the check is skipped: uncovered methods resolve through
// the manager's default selection, which is every registered scheme.
func (s *policySet) validateSchemeCoverage(registered []authentication.Scheme) error {
	if s.defaultDecision == SecurityDeny {
		return nil
	}
	referenced := make(map[authentication.Scheme]struct{})
	for _, candidate := range s.rules {
		for _, scheme := range candidate.rule.Authenticate {
			referenced[scheme] = struct{}{}
		}
	}
	for _, scheme := range registered {
		if _, ok := referenced[scheme]; !ok {
			return fmt.Errorf(
				"grpc: authentication scheme %q is registered but no security rule references it; name it in a rule or remove the plugin",
				scheme)
		}
	}
	return nil
}

// validateRuleSchemes rejects a rule naming a scheme no authenticator
// registers. It runs whether or not the rule matches a served method: a rule
// is configuration, and a misspelled scheme in it is a mistake even before a
// service it would govern is deployed.
func (s *policySet) validateRuleSchemes(registered []authentication.Scheme) error {
	known := make(map[authentication.Scheme]struct{}, len(registered))
	for _, scheme := range registered {
		known[scheme] = struct{}{}
	}
	for _, candidate := range s.rules {
		for _, scheme := range candidate.rule.Authenticate {
			if _, ok := known[scheme]; !ok {
				return fmt.Errorf("grpc: security rule %d (%q) names unknown scheme %q; registered schemes: %s",
					candidate.index, candidate.rule.Match, scheme, formatSchemeList(registered))
			}
		}
	}
	return nil
}

// validateDeclaredSchemes checks that declared is a full permutation of
// registered, naming the precise mistake.
func validateDeclaredSchemes(declared, registered []authentication.Scheme) error {
	registeredSet := make(map[authentication.Scheme]struct{}, len(registered))
	for _, scheme := range registered {
		registeredSet[scheme] = struct{}{}
	}
	seen := make(map[authentication.Scheme]struct{}, len(declared))
	for _, scheme := range declared {
		if _, duplicate := seen[scheme]; duplicate {
			return fmt.Errorf("xbc: grpc.security.schemes declares %q more than once; registered schemes: %s",
				scheme, formatSchemeList(registered))
		}
		seen[scheme] = struct{}{}
		if _, known := registeredSet[scheme]; !known {
			return fmt.Errorf("xbc: grpc.security.schemes declares unknown scheme %q; registered schemes: %s",
				scheme, formatSchemeList(registered))
		}
	}
	for _, scheme := range registered {
		if _, ok := seen[scheme]; !ok {
			return fmt.Errorf(
				"xbc: grpc.security.schemes is missing registered scheme %q; it must declare a full permutation of the registered authenticators: %s",
				scheme, formatSchemeList(registered))
		}
	}
	return nil
}

// formatSchemeList renders a scheme list for a diagnostic.
func formatSchemeList(schemes []authentication.Scheme) string {
	if len(schemes) == 0 {
		return "(none)"
	}
	parts := make([]string, len(schemes))
	for i, scheme := range schemes {
		parts[i] = string(scheme)
	}
	return strings.Join(parts, ", ")
}
