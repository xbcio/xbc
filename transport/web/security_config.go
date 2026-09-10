package web

import (
	"fmt"
	"strings"

	"github.com/xbcio/xbc/authentication"
)

// SecurityDefault is the closed set of fallback decisions applied to a frozen
// route that no explicit rule and no route-level policy covers.
type SecurityDefault string

const (
	// SecurityDeny rejects any request reaching authentication resolution
	// without a covering rule. It is the factory default.
	SecurityDeny SecurityDefault = "deny"
	// SecurityPermit lets uncovered routes through. It exists for services whose
	// endpoints are all legitimately public; enabling it logs a startup warning
	// because it turns the whole service fail-open.
	SecurityPermit SecurityDefault = "permit"
)

// SecurityConfig is the application's authoritative authentication policy. It
// is the highest of the three precedence tiers: an explicit rule here overrides
// a route's own .Auth() declaration, which is what makes the application -- not
// a plugin -- the final arbiter of which endpoints are public.
//
// Default deliberately does not participate in overriding. It applies only when
// neither an explicit rule nor a route-level policy covers a route, so adopting
// this configuration never silently closes routes that plugins declared public.
type SecurityConfig struct {
	Default  SecurityDefault `yaml:"default"  default:"deny"`
	Policies []PolicyRule    `yaml:"policies"`
}

// PolicyRule is one ordered matching rule. Rules are evaluated in declaration
// order and the first match wins, so a specific rule must be written before the
// broader rule it carves an exception out of.
//
// Permit and Authenticate are mutually exclusive and exactly one must be set:
// a rule that decides nothing is a configuration mistake, not a no-op.
type PolicyRule struct {
	Match        string                  `yaml:"match"`
	Permit       bool                    `yaml:"permit"`
	Authenticate []authentication.Scheme `yaml:"authenticate"`
}

// normalize fills in the restrictive default for a configuration section that
// omitted it. It does not validate; callers pair it with Validate.
func (c SecurityConfig) normalize() SecurityConfig {
	if c.Default == "" {
		c.Default = SecurityDeny
	}
	c.Policies = append([]PolicyRule(nil), c.Policies...)
	return c
}

// Validate reports configuration mistakes that must fail startup rather than
// degrade into a silently permissive policy.
func (c SecurityConfig) Validate() error {
	switch c.Default {
	case SecurityDeny, SecurityPermit:
	default:
		return fmt.Errorf("web: security default must be \"deny\" or \"permit\", got %q", c.Default)
	}

	for i, rule := range c.Policies {
		if strings.TrimSpace(rule.Match) == "" {
			return fmt.Errorf("web: security policy %d has an empty match", i)
		}
		if rule.Permit && len(rule.Authenticate) > 0 {
			return fmt.Errorf(
				"web: security policy %d (%q) cannot combine permit with authenticate",
				i, rule.Match,
			)
		}
		if !rule.Permit && len(rule.Authenticate) == 0 {
			return fmt.Errorf(
				"web: security policy %d (%q) must set either permit or authenticate",
				i, rule.Match,
			)
		}
		if _, err := parseMatch(rule.Match); err != nil {
			return fmt.Errorf("web: security policy %d: %w", i, err)
		}
		seen := make(map[authentication.Scheme]struct{}, len(rule.Authenticate))
		for j, scheme := range rule.Authenticate {
			if err := scheme.Validate(); err != nil {
				return fmt.Errorf(
					"web: security policy %d (%q) has an invalid scheme at index %d: %w",
					i, rule.Match, j, err,
				)
			}
			if _, duplicate := seen[scheme]; duplicate {
				return fmt.Errorf(
					"web: security policy %d (%q) has a duplicate authentication scheme %q",
					i, rule.Match, scheme,
				)
			}
			seen[scheme] = struct{}{}
		}
	}
	return nil
}
