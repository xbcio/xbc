package web

import (
	"fmt"
	"strings"
)

// wildcardSuffix is the only wildcard form the policy language accepts. Mid
// segment wildcards and negation are deliberately unsupported: an exception is
// expressed by writing the more specific rule before the broader one, which
// keeps first-match-wins the single thing an operator has to reason about.
const wildcardSuffix = "/**"

// matcher is a parsed policy match expression of the form "[METHOD ]path".
//
// path holds the literal prefix with any trailing "/**" removed, so a wildcard
// matcher for "/api/**" carries path "/api" and wildcard true.
type matcher struct {
	method   string
	path     string
	wildcard bool
}

// parseMatch parses one rule's match expression. It rejects every syntax the
// policy language does not support rather than silently treating it as a
// literal path, because a rule that never matches is indistinguishable from a
// missing rule at request time.
func parseMatch(raw string) (matcher, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return matcher{}, fmt.Errorf("empty match")
	}

	var method, pathPart string
	switch fields := strings.Fields(trimmed); len(fields) {
	case 1:
		pathPart = fields[0]
	case 2:
		method = strings.ToUpper(fields[0])
		pathPart = fields[1]
	default:
		return matcher{}, fmt.Errorf("match %q has too many fields, expected \"[METHOD ]path\"", raw)
	}

	if !strings.HasPrefix(pathPart, "/") {
		return matcher{}, fmt.Errorf("match path %q must start with \"/\"", pathPart)
	}

	wildcard := false
	if pathPart == wildcardSuffix {
		// "/**" is the root wildcard: prefix "" matches every absolute path.
		return matcher{method: method, path: "", wildcard: true}, nil
	}
	if strings.HasSuffix(pathPart, wildcardSuffix) {
		wildcard = true
		pathPart = strings.TrimSuffix(pathPart, wildcardSuffix)
	}
	if strings.Contains(pathPart, "*") {
		return matcher{}, fmt.Errorf(
			"match path %q supports only a trailing \"/**\" wildcard", raw,
		)
	}

	return matcher{method: method, path: pathPart, wildcard: wildcard}, nil
}

// matches reports whether this rule applies to a frozen route. path must be a
// RouteInfo.Path, not a raw request URL, so that policy resolution and
// CurrentRoute agree on one path vocabulary.
func (m matcher) matches(method, path string) bool {
	if m.method != "" && !strings.EqualFold(m.method, method) {
		return false
	}
	if !m.wildcard {
		return m.path == path
	}
	if m.path == "" {
		return strings.HasPrefix(path, "/")
	}
	if path == m.path {
		return true
	}
	return strings.HasPrefix(path, m.path+"/")
}

// covers reports whether every route matched by other is also matched by m.
// Startup validation uses this to find a rule that can never be reached
// because an earlier, broader rule already decided every route it names.
func (m matcher) covers(other matcher) bool {
	if m.method != "" && !strings.EqualFold(m.method, other.method) {
		return false
	}
	if !m.wildcard {
		return !other.wildcard && m.path == other.path
	}
	if m.path == "" {
		return true
	}
	if other.path == m.path {
		return true
	}
	return strings.HasPrefix(other.path, m.path+"/")
}
