package tenant

import (
	"context"
	"fmt"
	"strings"

	"github.com/xbcio/xbc/transport/web"
)

type principalResolver struct {
	cfg normalizedConfig
}

func (r principalResolver) ResolveTenant(ctx context.Context, principal web.Principal, requestedID string) (Tenant, bool, error) {
	if ctx == nil {
		return Tenant{}, false, tenantError("resolver context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return Tenant{}, false, err
	}
	if strings.TrimSpace(principal.Subject) == "" {
		return Tenant{}, false, nil
	}

	members := make([]string, 0, 4)
	seen := make(map[string]struct{})
	add := func(value string) error {
		trimmed := strings.TrimSpace(value)
		if trimmed != value || !validTenantID(trimmed, r.cfg.minIDLength, r.cfg.maxIDLength) {
			return fmt.Errorf("tenant: principal contains an invalid tenant ID")
		}
		if _, duplicate := seen[trimmed]; !duplicate {
			seen[trimmed] = struct{}{}
			members = append(members, trimmed)
		}
		return nil
	}

	if raw, exists := principal.Attributes[r.cfg.tenantIDAttribute]; exists && raw != nil {
		value, ok := raw.(string)
		if !ok || add(value) != nil {
			return Tenant{}, false, tenantError("principal tenant_id attribute is invalid")
		}
	}
	if raw, exists := principal.Attributes[r.cfg.tenantIDsAttribute]; exists && raw != nil {
		values, ok := stringValues(raw)
		if !ok {
			return Tenant{}, false, tenantError("principal tenant_ids attribute is invalid")
		}
		for _, value := range values {
			if err := add(value); err != nil {
				return Tenant{}, false, err
			}
		}
	}

	selected := strings.TrimSpace(requestedID)
	if selected != requestedID {
		return Tenant{}, false, nil
	}
	if selected != "" {
		if _, member := seen[selected]; !member {
			return Tenant{}, false, nil
		}
	} else {
		if !r.cfg.autoSelectSingle || len(members) != 1 {
			return Tenant{}, false, nil
		}
		selected = members[0]
	}
	return Tenant{ID: selected, Attributes: tenantAttributes(principal.Attributes[r.cfg.tenantAttributesAttribute], selected)}, true, nil
}

func stringValues(value any) ([]string, bool) {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...), true
	case []any:
		values := make([]string, len(typed))
		for i, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, false
			}
			values[i] = text
		}
		return values, true
	default:
		return nil, false
	}
}

func tenantAttributes(value any, id string) map[string]any {
	byTenant, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	attributes, ok := byTenant[id].(map[string]any)
	if !ok {
		return nil
	}
	return cloneAttributes(attributes)
}
