package tenant

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

const tenantContextKey = "xbc/transport/web/tenant.current"

// Tenant is the verified request tenancy. Attributes are optional facts from
// the trusted Resolver and are defensively copied on publication and reads.
type Tenant struct {
	ID         string
	Attributes map[string]any
}

// Resolver converts a verified Principal plus an untrusted requested ID into
// one authorized tenant. requestedID is empty when no selection header was
// supplied. Implementations must never treat requestedID itself as proof of
// membership. found=false means the principal has no unambiguous selection.
type Resolver interface {
	ResolveTenant(ctx context.Context, principal web.Principal, requestedID string) (resolved Tenant, found bool, err error)
}

// ResolverFunc adapts a function to Resolver.
type ResolverFunc func(context.Context, web.Principal, string) (Tenant, bool, error)

func (f ResolverFunc) ResolveTenant(ctx context.Context, principal web.Principal, requestedID string) (Tenant, bool, error) {
	if f == nil {
		return Tenant{}, false, errors.New("tenant: nil resolver function")
	}
	return f(ctx, principal, requestedID)
}

// Current returns the trusted request tenant as a defensive deep copy.
func Current(c *gin.Context) (Tenant, bool) {
	if c == nil {
		return Tenant{}, false
	}
	value, exists := c.Get(tenantContextKey)
	if !exists {
		return Tenant{}, false
	}
	resolved, ok := value.(Tenant)
	if !ok || strings.TrimSpace(resolved.ID) == "" {
		return Tenant{}, false
	}
	return cloneTenant(resolved), true
}

// Set publishes a tenant already verified by trusted application code. It
// validates the conservative default ID grammar and makes a defensive copy.
// HTTP header values must never be passed here without membership validation;
// normal applications should let this plugin's middleware publish instead.
func Set(c *gin.Context, resolved Tenant) bool {
	if c == nil {
		return false
	}
	trimmed := strings.TrimSpace(resolved.ID)
	if trimmed != resolved.ID || !validTenantID(trimmed, 1, defaultMaxIDLength) {
		return false
	}
	resolved.ID = trimmed
	resolved.Attributes = cloneAttributes(resolved.Attributes)
	c.Set(tenantContextKey, resolved)
	return true
}

func setValidated(c *gin.Context, resolved Tenant, minimum, maximum int) bool {
	if c == nil {
		return false
	}
	trimmed := strings.TrimSpace(resolved.ID)
	if trimmed != resolved.ID || !validTenantID(trimmed, minimum, maximum) {
		return false
	}
	resolved.ID = trimmed
	resolved.Attributes = cloneAttributes(resolved.Attributes)
	c.Set(tenantContextKey, resolved)
	return true
}

func cloneTenant(value Tenant) Tenant {
	value.Attributes = cloneAttributes(value.Attributes)
	return value
}

func cloneAttributes(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	copy := make(map[string]any, len(source))
	for key, value := range source {
		copy[key] = cloneValue(value)
	}
	return copy
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneAttributes(typed)
	case []any:
		copy := make([]any, len(typed))
		for i := range typed {
			copy[i] = cloneValue(typed[i])
		}
		return copy
	case []string:
		return append([]string(nil), typed...)
	case []byte:
		return append([]byte(nil), typed...)
	default:
		return value
	}
}

func validTenantID(id string, minimum, maximum int) bool {
	if len(id) < minimum || len(id) > maximum {
		return false
	}
	for i := range len(id) {
		ch := id[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			continue
		}
		switch ch {
		case '-', '_', '.', ':':
			continue
		default:
			return false
		}
	}
	return true
}

func tenantError(message string) error { return fmt.Errorf("tenant: %s", message) }
