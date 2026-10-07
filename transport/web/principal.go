package web

import (
	"strings"

	"github.com/xbcio/xbc/extensions/authentication"
)

const principalContextKey = "xbc/transport/web.principal"

// SetPrincipal publishes a verified identity on the current request. It
// rejects nil contexts and empty subjects, returning false rather than
// installing an identity that downstream authorization could misinterpret.
func SetPrincipal(c *Ctx, principal authentication.Principal) bool {
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
func CurrentPrincipal(c *Ctx) (authentication.Principal, bool) {
	if c == nil {
		return authentication.Principal{}, false
	}
	value, ok := c.Get(principalContextKey)
	if !ok {
		return authentication.Principal{}, false
	}
	principal, ok := value.(authentication.Principal)
	if !ok || strings.TrimSpace(principal.Subject) == "" {
		return authentication.Principal{}, false
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
