package casbin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	casbinlib "github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"
	"github.com/casbin/casbin/v2/persist"

	"github.com/xbcio/xbc/extensions/authorization/rbac"
)

const (
	rbacRequestFields  = 3
	rbacPolicyFields   = 3
	rbacGroupingFields = 2
)

var _ rbac.Backend = (*Plugin)(nil)

// Capabilities describes the model shape and persistent mutation support of
// this Plugin. Inline and file-backed policy sources are intentionally
// reported as read-only even though Casbin can mutate their in-memory model.
func (p *Plugin) Capabilities() rbac.Capabilities {
	if p == nil || p.state == nil || p.state.enforcer == nil {
		return rbac.Capabilities{}
	}

	enforcer := p.state.enforcer
	m := enforcer.GetModel()
	capabilities := rbac.Capabilities{
		RequestFields:  assertionFields(m, "r", "r"),
		PolicyFields:   assertionFields(m, "p", "p"),
		GroupingFields: assertionFields(m, "g", "g"),
	}
	adapter := enforcer.GetAdapter()
	if p.state.cfg.adapter.enabled() && !isNil(adapter) {
		_, capabilities.Mutable = adapter.(persist.BatchAdapter)
		_, capabilities.FilteredPolicyReplace = adapter.(persist.UpdatableAdapter)
	}
	return capabilities
}

func assertionFields(m model.Model, section, name string) int {
	assertions, ok := m[section]
	if !ok {
		return 0
	}
	assertion, ok := assertions[name]
	if !ok || assertion == nil {
		return 0
	}
	return len(assertion.Tokens)
}

// Authorize evaluates one normalized subject/object/action request.
func (p *Plugin) Authorize(ctx context.Context, subject string, permission rbac.Permission) (bool, error) {
	enforcer, err := p.rbacEnforcer(ctx, false, false)
	if err != nil {
		return false, err
	}
	subject, err = normalizeRequired("subject", subject)
	if err != nil {
		return false, err
	}
	permission, err = normalizePermission(permission)
	if err != nil {
		return false, err
	}

	allowed, err := enforcer.Enforce(subject, permission.Object, permission.Action)
	if err != nil {
		return false, fmt.Errorf("casbin: authorize subject %q: %w", subject, err)
	}
	return allowed, nil
}

// HasRole reports whether subject has role as a direct Casbin grouping
// relationship.
func (p *Plugin) HasRole(ctx context.Context, subject, role string) (bool, error) {
	enforcer, err := p.rbacEnforcer(ctx, false, false)
	if err != nil {
		return false, err
	}
	subject, err = normalizeRequired("subject", subject)
	if err != nil {
		return false, err
	}
	role, err = normalizeRequired("role", role)
	if err != nil {
		return false, err
	}

	hasRole, err := enforcer.HasRoleForUser(subject, role)
	if err != nil {
		return false, fmt.Errorf("casbin: check role %q for subject %q: %w", role, subject, err)
	}
	return hasRole, nil
}

// Roles returns subject's direct roles, de-duplicated and sorted.
func (p *Plugin) Roles(ctx context.Context, subject string) ([]string, error) {
	enforcer, err := p.rbacEnforcer(ctx, false, false)
	if err != nil {
		return nil, err
	}
	subject, err = normalizeRequired("subject", subject)
	if err != nil {
		return nil, err
	}
	return rolesForSubject(enforcer, subject)
}

func rolesForSubject(enforcer *casbinlib.SyncedEnforcer, subject string) ([]string, error) {
	roles, err := enforcer.GetRolesForUser(subject)
	if err != nil {
		return nil, fmt.Errorf("casbin: list roles for subject %q: %w", subject, err)
	}
	normalized, err := normalizeRoles(roles)
	if err != nil {
		return nil, fmt.Errorf("casbin: list roles for subject %q: %w", subject, err)
	}
	return normalized, nil
}

// RolePermissions returns role's direct policy permissions, de-duplicated and
// sorted by object then action.
func (p *Plugin) RolePermissions(ctx context.Context, role string) ([]rbac.Permission, error) {
	enforcer, err := p.rbacEnforcer(ctx, false, false)
	if err != nil {
		return nil, err
	}
	role, err = normalizeRequired("role", role)
	if err != nil {
		return nil, err
	}
	return permissionsForRole(enforcer, role)
}

func permissionsForRole(enforcer *casbinlib.SyncedEnforcer, role string) ([]rbac.Permission, error) {
	policies, err := enforcer.GetPermissionsForUser(role)
	if err != nil {
		return nil, fmt.Errorf("casbin: list permissions for role %q: %w", role, err)
	}
	permissions := make([]rbac.Permission, 0, len(policies))
	for _, policy := range policies {
		if len(policy) != rbacPolicyFields {
			return nil, fmt.Errorf("casbin: list permissions for role %q: policy has %d fields, want %d", role, len(policy), rbacPolicyFields)
		}
		if strings.TrimSpace(policy[0]) != role {
			return nil, fmt.Errorf("casbin: list permissions for role %q: policy subject is %q", role, policy[0])
		}
		permissions = append(permissions, rbac.Permission{Object: policy[1], Action: policy[2]})
	}
	normalized, err := normalizePermissions(permissions)
	if err != nil {
		return nil, fmt.Errorf("casbin: list permissions for role %q: %w", role, err)
	}
	return normalized, nil
}

// ReplaceRolePermissions converges role's direct policy set. Existing and
// target non-empty sets use the adapter's filtered update operation; initial
// and clearing writes retain Casbin's normal watcher notification paths.
func (p *Plugin) ReplaceRolePermissions(ctx context.Context, role string, permissions []rbac.Permission) (bool, error) {
	if _, err := p.rbacEnforcer(ctx, true, true); err != nil {
		return false, err
	}
	role, err := normalizeRequired("role", role)
	if err != nil {
		return false, err
	}
	permissions, err = normalizePermissions(permissions)
	if err != nil {
		return false, err
	}

	p.mutationMu.Lock()
	defer p.mutationMu.Unlock()
	enforcer, err := p.rbacEnforcer(ctx, true, true)
	if err != nil {
		return false, err
	}
	current, err := permissionsForRole(enforcer, role)
	if err != nil {
		return false, err
	}
	if equalPermissions(current, permissions) {
		return false, nil
	}

	rules := make([][]string, len(permissions))
	for index, permission := range permissions {
		rules[index] = []string{role, permission.Object, permission.Action}
	}

	var changed bool
	switch {
	case len(current) == 0:
		if err := requireBatchAdapter(enforcer); err != nil {
			return false, err
		}
		changed, err = enforcer.AddPoliciesEx(rules)
	case len(permissions) == 0:
		changed, err = enforcer.RemoveFilteredPolicy(0, role)
	default:
		changed, err = enforcer.UpdateFilteredPolicies(rules, 0, role)
	}
	if err != nil {
		if changed {
			return true, fmt.Errorf("casbin: replace permissions for role %q (change may already be persisted before notification failed): %w", role, err)
		}
		return false, fmt.Errorf("casbin: replace permissions for role %q: %w", role, err)
	}
	if !changed {
		return false, fmt.Errorf("casbin: replace permissions for role %q reported no change after the policy sets differed", role)
	}
	return true, nil
}

// ReplaceSubjectRoles converges subject's direct grouping relationships. Casbin
// has no filtered grouping replace primitive, so this operation is serialized
// per Plugin and deliberately does not claim storage-level atomicity. A failed
// remove or add triggers a best-effort restoration and policy reload.
func (p *Plugin) ReplaceSubjectRoles(ctx context.Context, subject string, roles []string) (bool, error) {
	if _, err := p.rbacEnforcer(ctx, true, false); err != nil {
		return false, err
	}
	subject, err := normalizeRequired("subject", subject)
	if err != nil {
		return false, err
	}
	roles, err = normalizeRoles(roles)
	if err != nil {
		return false, err
	}

	p.mutationMu.Lock()
	defer p.mutationMu.Unlock()
	enforcer, err := p.rbacEnforcer(ctx, true, false)
	if err != nil {
		return false, err
	}
	current, err := rolesForSubject(enforcer, subject)
	if err != nil {
		return false, err
	}
	if equalStrings(current, roles) {
		return false, nil
	}
	if len(roles) > 0 {
		if err := requireBatchAdapter(enforcer); err != nil {
			return false, err
		}
	}

	if len(current) > 0 {
		removed, removeErr := enforcer.DeleteRolesForUser(subject)
		if removeErr != nil {
			return p.subjectRoleReplaceFailure(enforcer, subject, current, fmt.Errorf("remove current roles: %w", removeErr))
		}
		if !removed {
			return p.subjectRoleReplaceFailure(enforcer, subject, current, errors.New("remove current roles reported no change"))
		}
	}
	if len(roles) > 0 {
		added, addErr := enforcer.AddRolesForUser(subject, roles)
		if addErr != nil {
			return p.subjectRoleReplaceFailure(enforcer, subject, current, fmt.Errorf("add target roles: %w", addErr))
		}
		if !added {
			return p.subjectRoleReplaceFailure(enforcer, subject, current, errors.New("add target roles reported no change"))
		}
	}
	return true, nil
}

func (p *Plugin) subjectRoleReplaceFailure(enforcer *casbinlib.SyncedEnforcer, subject string, previous []string, cause error) (bool, error) {
	recoveryErr := restoreSubjectRoles(enforcer, subject, previous)
	joined := cause
	if recoveryErr != nil {
		joined = errors.Join(cause, recoveryErr)
	}
	return false, fmt.Errorf("casbin: replace roles for subject %q failed; the operation is not storage-atomic and restoration was attempted: %w", subject, joined)
}

func restoreSubjectRoles(enforcer *casbinlib.SyncedEnforcer, subject string, previous []string) error {
	var recoveryErrors []error
	if _, err := enforcer.DeleteRolesForUser(subject); err != nil {
		recoveryErrors = append(recoveryErrors, fmt.Errorf("clear partial roles during restoration: %w", err))
	}
	if len(previous) > 0 {
		if _, err := enforcer.AddRolesForUser(subject, previous); err != nil {
			recoveryErrors = append(recoveryErrors, fmt.Errorf("restore previous roles: %w", err))
		}
	}
	if err := enforcer.LoadPolicy(); err != nil {
		recoveryErrors = append(recoveryErrors, fmt.Errorf("reload policy after restoration: %w", err))
		return errors.Join(recoveryErrors...)
	}
	restored, err := rolesForSubject(enforcer, subject)
	if err != nil {
		recoveryErrors = append(recoveryErrors, fmt.Errorf("verify restored roles: %w", err))
	} else if !equalStrings(restored, previous) {
		recoveryErrors = append(recoveryErrors, fmt.Errorf("verify restored roles: got %v, want %v", restored, previous))
	}
	return errors.Join(recoveryErrors...)
}

// DeleteSubject removes the subject's direct grouping and policy rules through
// Casbin's generic management API.
func (p *Plugin) DeleteSubject(ctx context.Context, subject string) (bool, error) {
	if _, err := p.rbacEnforcer(ctx, true, false); err != nil {
		return false, err
	}
	subject, err := normalizeRequired("subject", subject)
	if err != nil {
		return false, err
	}

	p.mutationMu.Lock()
	defer p.mutationMu.Unlock()
	enforcer, err := p.rbacEnforcer(ctx, true, false)
	if err != nil {
		return false, err
	}
	changed, err := enforcer.DeleteUser(subject)
	if err != nil {
		return changed, mutationFailure("delete subject", subject, changed, err)
	}
	return changed, nil
}

// DeleteRole removes the role's incoming/outgoing grouping and policy rules
// through Casbin's generic management API.
func (p *Plugin) DeleteRole(ctx context.Context, role string) (bool, error) {
	if _, err := p.rbacEnforcer(ctx, true, false); err != nil {
		return false, err
	}
	role, err := normalizeRequired("role", role)
	if err != nil {
		return false, err
	}

	p.mutationMu.Lock()
	defer p.mutationMu.Unlock()
	enforcer, err := p.rbacEnforcer(ctx, true, false)
	if err != nil {
		return false, err
	}
	changed, err := enforcer.DeleteRole(role)
	if err != nil {
		return changed, mutationFailure("delete role", role, changed, err)
	}
	return changed, nil
}

func mutationFailure(operation, value string, changed bool, err error) error {
	if changed {
		return fmt.Errorf("casbin: %s %q (change may already be persisted before notification failed): %w", operation, value, err)
	}
	return fmt.Errorf("casbin: %s %q: %w", operation, value, err)
}

func (p *Plugin) rbacEnforcer(ctx context.Context, mutable, filteredReplace bool) (*casbinlib.SyncedEnforcer, error) {
	if ctx == nil {
		return nil, errors.New("casbin: RBAC backend requires a non-nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("casbin: RBAC backend context: %w", err)
	}
	if p == nil || p.state == nil || p.state.enforcer == nil {
		return nil, errors.New("casbin: RBAC backend is not initialized")
	}
	if p.stopped.Load() {
		return nil, ErrStopped
	}

	capabilities := p.Capabilities()
	if capabilities.RequestFields != rbacRequestFields || capabilities.PolicyFields != rbacPolicyFields || capabilities.GroupingFields != rbacGroupingFields {
		return nil, fmt.Errorf(
			"casbin: RBAC backend requires r/p/g field counts %d/%d/%d, got %d/%d/%d",
			rbacRequestFields,
			rbacPolicyFields,
			rbacGroupingFields,
			capabilities.RequestFields,
			capabilities.PolicyFields,
			capabilities.GroupingFields,
		)
	}
	if mutable && !capabilities.Mutable {
		return nil, errors.New("casbin: RBAC backend policy source is read-only or its adapter lacks batch operations; persistent RBAC mutation requires an external batch adapter")
	}
	if filteredReplace && !capabilities.FilteredPolicyReplace {
		return nil, errors.New("casbin: RBAC backend adapter does not support filtered policy replacement")
	}
	return p.state.enforcer, nil
}

func requireBatchAdapter(enforcer *casbinlib.SyncedEnforcer) error {
	adapter := enforcer.GetAdapter()
	if isNil(adapter) {
		return errors.New("casbin: RBAC backend has no active adapter")
	}
	if _, ok := adapter.(persist.BatchAdapter); !ok {
		return fmt.Errorf("casbin: RBAC backend adapter %T does not support batch policy additions", adapter)
	}
	return nil
}

func normalizeRequired(field, value string) (string, error) {
	normalized := strings.TrimSpace(value)
	if normalized == "" {
		return "", fmt.Errorf("casbin: RBAC backend %s must not be empty", field)
	}
	return normalized, nil
}

func normalizePermission(permission rbac.Permission) (rbac.Permission, error) {
	object, err := normalizeRequired("permission object", permission.Object)
	if err != nil {
		return rbac.Permission{}, err
	}
	action := strings.TrimSpace(permission.Action)
	if action == "" {
		action = "*"
	}
	return rbac.Permission{Object: object, Action: action}, nil
}

func normalizeRoles(roles []string) ([]string, error) {
	unique := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		normalized, err := normalizeRequired("role", role)
		if err != nil {
			return nil, err
		}
		unique[normalized] = struct{}{}
	}
	result := make([]string, 0, len(unique))
	for role := range unique {
		result = append(result, role)
	}
	sort.Strings(result)
	return result, nil
}

func normalizePermissions(permissions []rbac.Permission) ([]rbac.Permission, error) {
	unique := make(map[rbac.Permission]struct{}, len(permissions))
	for _, permission := range permissions {
		normalized, err := normalizePermission(permission)
		if err != nil {
			return nil, err
		}
		unique[normalized] = struct{}{}
	}
	result := make([]rbac.Permission, 0, len(unique))
	for permission := range unique {
		result = append(result, permission)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Object != result[j].Object {
			return result[i].Object < result[j].Object
		}
		return result[i].Action < result[j].Action
	})
	return result, nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalPermissions(left, right []rbac.Permission) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
