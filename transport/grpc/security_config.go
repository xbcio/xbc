package grpc

import (
	"fmt"
	"strings"

	"github.com/xbcio/xbc/extensions/authentication"
)

// SecurityDefault is the closed set of fallback decisions applied to a frozen
// method that no explicit rule and no registration-time declaration covers.
type SecurityDefault string

const (
	// SecurityDeny authenticates any method without a covering rule through
	// every registered scheme. It is the factory default.
	SecurityDeny SecurityDefault = "deny"
	// SecurityPermit lets uncovered methods through. It exists for services
	// whose methods are all legitimately public; enabling it logs a startup
	// warning because it turns the whole service fail-open.
	SecurityPermit SecurityDefault = "permit"
)

// SecurityConfig is the application's authoritative authentication policy for
// the gRPC server, the "grpc.security" section. It is the highest of the three
// precedence tiers: an explicit rule here overrides a method's own
// registration-time declaration, which is what makes the application -- not a
// plugin -- the final arbiter of which methods are public.
//
// Default deliberately does not participate in overriding. It applies only when
// neither an explicit rule nor a registration-time declaration covers a method,
// so adopting this configuration never silently closes methods that plugins
// declared public.
type SecurityConfig struct {
	Default SecurityDefault `yaml:"default" default:"deny"`

	// Schemes is the application-declared arbitration order for authentication.
	// The authentication manager evaluates schemes first-applicable in exactly
	// this order, so the order is a security decision: a scheme that must win
	// over another must be written before it. It must be a full permutation of
	// the registered authenticators, and may be omitted only when exactly one is
	// registered.
	Schemes []authentication.Scheme `yaml:"schemes"`

	Rules []SecurityRule `yaml:"rules"`
}

// SecurityRule is one ordered matching rule. Rules are evaluated in declaration
// order and the first match wins, so a specific rule must be written before the
// broader rule it carves an exception out of.
//
// Match names gRPC methods by full method name: "/pkg.Service/Method" names one
// method, "/pkg.Service/*" every method of one service, and "/**" every method
// the server serves. No other wildcard form is accepted.
//
// Permit and Authenticate are mutually exclusive and exactly one must be set: a
// rule that decides nothing is a configuration mistake, not a no-op.
type SecurityRule struct {
	Match        string                  `yaml:"match"`
	Permit       bool                    `yaml:"permit"`
	Authenticate []authentication.Scheme `yaml:"authenticate"`
}

// normalize fills in the restrictive default for a section that omitted it and
// detaches the slices from the caller's. It does not validate.
func (c SecurityConfig) normalize() SecurityConfig {
	if c.Default == "" {
		c.Default = SecurityDeny
	}
	c.Schemes = append([]authentication.Scheme(nil), c.Schemes...)
	c.Rules = append([]SecurityRule(nil), c.Rules...)
	for i := range c.Rules {
		c.Rules[i].Authenticate = append([]authentication.Scheme(nil), c.Rules[i].Authenticate...)
	}
	return c
}

// Validate reports configuration mistakes that must fail startup rather than
// degrade into a silently permissive policy. Checks that need the registered
// authenticators or the frozen method table run during assembly.
func (c SecurityConfig) Validate() error {
	switch c.Default {
	case SecurityDeny, SecurityPermit:
	default:
		return fmt.Errorf("grpc: security default must be \"deny\" or \"permit\", got %q", c.Default)
	}

	seenScheme := make(map[authentication.Scheme]struct{}, len(c.Schemes))
	for i, scheme := range c.Schemes {
		if err := scheme.Validate(); err != nil {
			return fmt.Errorf("grpc: security schemes has an invalid scheme at index %d: %w", i, err)
		}
		if _, duplicate := seenScheme[scheme]; duplicate {
			return fmt.Errorf("grpc: security schemes has a duplicate scheme %q at index %d", scheme, i)
		}
		seenScheme[scheme] = struct{}{}
	}

	for i, rule := range c.Rules {
		if strings.TrimSpace(rule.Match) == "" {
			return fmt.Errorf("grpc: security rule %d has an empty match", i)
		}
		if rule.Permit && len(rule.Authenticate) > 0 {
			return fmt.Errorf("grpc: security rule %d (%q) cannot combine permit with authenticate", i, rule.Match)
		}
		if !rule.Permit && len(rule.Authenticate) == 0 {
			return fmt.Errorf("grpc: security rule %d (%q) must set either permit or authenticate", i, rule.Match)
		}
		if _, err := parseMethodMatch(rule.Match); err != nil {
			return fmt.Errorf("grpc: security rule %d: %w", i, err)
		}
		seen := make(map[authentication.Scheme]struct{}, len(rule.Authenticate))
		for j, scheme := range rule.Authenticate {
			if err := scheme.Validate(); err != nil {
				return fmt.Errorf("grpc: security rule %d (%q) has an invalid scheme at index %d: %w", i, rule.Match, j, err)
			}
			if _, duplicate := seen[scheme]; duplicate {
				return fmt.Errorf("grpc: security rule %d (%q) has a duplicate authentication scheme %q", i, rule.Match, scheme)
			}
			seen[scheme] = struct{}{}
		}
	}
	return nil
}

// methodMatcher is a parsed rule match. service is empty only for the "/**"
// wildcard; method is empty when the matcher names a whole service.
type methodMatcher struct {
	service string
	method  string
	all     bool
}

// parseMethodMatch parses one rule's match expression. It rejects every syntax
// the policy language does not support rather than treating it as a literal,
// because a rule that never matches is indistinguishable from a missing rule
// at request time.
func parseMethodMatch(raw string) (methodMatcher, error) {
	match := strings.TrimSpace(raw)
	if match != raw {
		return methodMatcher{}, fmt.Errorf("match %q must not carry surrounding whitespace", raw)
	}
	if match == "/**" {
		return methodMatcher{all: true}, nil
	}
	service, method, ok := strings.Cut(strings.TrimPrefix(match, "/"), "/")
	if !strings.HasPrefix(match, "/") || !ok || service == "" || method == "" ||
		strings.ContainsAny(service, "/* \t") || strings.Contains(method, "/") {
		return methodMatcher{}, fmt.Errorf(
			"match %q must be \"/pkg.Service/Method\", \"/pkg.Service/*\", or \"/**\"", raw)
	}
	if method == "*" {
		return methodMatcher{service: service}, nil
	}
	if strings.ContainsAny(method, "* \t") {
		return methodMatcher{}, fmt.Errorf("match %q supports only a whole-method \"*\" wildcard", raw)
	}
	return methodMatcher{service: service, method: method}, nil
}

// matches reports whether this rule applies to one frozen method.
func (m methodMatcher) matches(service, method string) bool {
	if m.all {
		return true
	}
	if m.service != service {
		return false
	}
	return m.method == "" || m.method == method
}

// covers reports whether every method matched by other is also matched by m.
// Assembly uses it to reject a rule that an earlier, broader rule makes
// unreachable.
func (m methodMatcher) covers(other methodMatcher) bool {
	if m.all {
		return true
	}
	if other.all || m.service != other.service {
		return false
	}
	return m.method == "" || m.method == other.method
}
