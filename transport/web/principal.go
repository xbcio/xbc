package web

import (
	"strings"
)

const principalContextKey = "xbc/transport/web.principal"

// Principal is the transport-neutral result of successful HTTP
// authentication. Authentication plugins publish one Principal; authorization,
// auditing, and other business middleware consume it without importing a
// concrete JWT, API-key, or session implementation.
//
// Subject is the stable policy/audit identity. AuthMethod identifies how the
// request authenticated (for example "jwt" or "apikey"). Attributes are
// optional verified facts; callers receive defensive map copies so they cannot
// replace another middleware's top-level values accidentally.
type Principal struct {
	Subject    string
	AuthMethod string
	Attributes map[string]any
}

// SetPrincipal publishes a verified identity on the current request. It
// rejects nil contexts and empty subjects, returning false rather than
// installing an identity that downstream authorization could misinterpret.
func SetPrincipal(c *Ctx, principal Principal) bool {
	if c == nil {
		return false
	}
	principal.Subject = strings.TrimSpace(principal.Subject)
	if principal.Subject == "" {
		return false
	}
	principal.AuthMethod = strings.TrimSpace(principal.AuthMethod)
	principal.Attributes = cloneAttributes(principal.Attributes)
	c.Set(principalContextKey, principal)
	return true
}

// CurrentPrincipal returns the verified identity published by an upstream
// authentication plugin. The returned Attributes map is a defensive copy.
func CurrentPrincipal(c *Ctx) (Principal, bool) {
	if c == nil {
		return Principal{}, false
	}
	value, ok := c.Get(principalContextKey)
	if !ok {
		return Principal{}, false
	}
	principal, ok := value.(Principal)
	if !ok || strings.TrimSpace(principal.Subject) == "" {
		return Principal{}, false
	}
	principal.Attributes = cloneAttributes(principal.Attributes)
	return principal, true
}

func cloneAttributes(attributes map[string]any) map[string]any {
	if attributes == nil {
		return nil
	}
	cloned := make(map[string]any, len(attributes))
	for key, value := range attributes {
		cloned[key] = value
	}
	return cloned
}
