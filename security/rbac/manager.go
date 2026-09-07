package rbac

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/xbcio/xbc/plugin"
)

var (
	// ErrInvalidInput identifies a subject, role, permission, or context that
	// cannot be normalized into an RBAC operation.
	ErrInvalidInput = errors.New("rbac: invalid input")
	// ErrIncompatibleBackend identifies a nil backend or one whose reported
	// model and mutation capabilities cannot implement Manager semantics.
	ErrIncompatibleBackend = errors.New("rbac: incompatible backend")
)

var requiredCapabilities = Capabilities{
	RequestFields:         3,
	PolicyFields:          3,
	GroupingFields:        2,
	Mutable:               true,
	FilteredPolicyReplace: true,
}

type manager struct {
	backend         Backend
	backendIdentity plugin.Identity
	adminRole       string
}

var _ Manager = (*manager)(nil)

// New constructs a Manager over backend. Config.Backend labels the injected
// backend for diagnostics; Definition-driven composition additionally uses it
// to resolve that exact provider instance.
func New(config Config, backend Backend) (Manager, error) {
	normalized, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	return newManager(normalized, normalized.backend, backend)
}

func newManager(config normalizedConfig, identity plugin.Identity, backend Backend) (*manager, error) {
	identity = identity.Normalized()
	if isNilInterface(backend) {
		return nil, fmt.Errorf("%w: backend %s is nil", ErrIncompatibleBackend, identity)
	}
	capabilities := backend.Capabilities()
	if capabilities != requiredCapabilities {
		return nil, fmt.Errorf(
			"%w: backend %s reports capabilities %+v; require %+v",
			ErrIncompatibleBackend,
			identity,
			capabilities,
			requiredCapabilities,
		)
	}
	return &manager{
		backend:         backend,
		backendIdentity: identity,
		adminRole:       config.adminRole,
	}, nil
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func (m *manager) IsAdmin(ctx context.Context, subject string) (bool, error) {
	normalizedSubject, err := normalizeRequiredInput("subject", subject)
	if err != nil {
		return false, err
	}
	return m.isAdmin(ctx, normalizedSubject)
}

func (m *manager) Authorize(ctx context.Context, subject string, permission Permission) (bool, error) {
	normalizedSubject, err := normalizeRequiredInput("subject", subject)
	if err != nil {
		return false, err
	}
	normalizedPermission, err := normalizePermissionInput(permission)
	if err != nil {
		return false, err
	}
	if err := checkContext(ctx); err != nil {
		return false, err
	}

	admin, err := m.isAdmin(ctx, normalizedSubject)
	if err != nil || admin {
		return admin, err
	}
	return m.authorize(ctx, normalizedSubject, normalizedPermission)
}

func (m *manager) AuthorizeAll(ctx context.Context, subject string, permissions ...Permission) (bool, error) {
	normalizedSubject, err := normalizeRequiredInput("subject", subject)
	if err != nil {
		return false, err
	}
	normalizedPermissions, err := normalizePermissionInputs(permissions)
	if err != nil {
		return false, err
	}
	if err := checkContext(ctx); err != nil {
		return false, err
	}
	if len(normalizedPermissions) == 0 {
		return true, nil
	}

	admin, err := m.isAdmin(ctx, normalizedSubject)
	if err != nil || admin {
		return admin, err
	}
	for _, permission := range normalizedPermissions {
		allowed, authorizeErr := m.authorize(ctx, normalizedSubject, permission)
		if authorizeErr != nil || !allowed {
			return allowed, authorizeErr
		}
	}
	return true, nil
}

func (m *manager) AuthorizeAny(ctx context.Context, subject string, permissions ...Permission) (bool, error) {
	normalizedSubject, err := normalizeRequiredInput("subject", subject)
	if err != nil {
		return false, err
	}
	normalizedPermissions, err := normalizePermissionInputs(permissions)
	if err != nil {
		return false, err
	}
	if err := checkContext(ctx); err != nil {
		return false, err
	}
	if len(normalizedPermissions) == 0 {
		return false, nil
	}

	admin, err := m.isAdmin(ctx, normalizedSubject)
	if err != nil || admin {
		return admin, err
	}
	for _, permission := range normalizedPermissions {
		allowed, authorizeErr := m.authorize(ctx, normalizedSubject, permission)
		if authorizeErr != nil || allowed {
			return allowed, authorizeErr
		}
	}
	return false, nil
}

func (m *manager) HasRole(ctx context.Context, subject, role string) (bool, error) {
	normalizedSubject, err := normalizeRequiredInput("subject", subject)
	if err != nil {
		return false, err
	}
	normalizedRole, err := normalizeRequiredInput("role", role)
	if err != nil {
		return false, err
	}
	if err := checkContext(ctx); err != nil {
		return false, err
	}

	hasRole, err := m.backend.HasRole(ctx, normalizedSubject, normalizedRole)
	if err != nil {
		return false, fmt.Errorf("rbac: backend %s check role %q for subject %q: %w", m.backendIdentity, normalizedRole, normalizedSubject, err)
	}
	return hasRole, nil
}

func (m *manager) Roles(ctx context.Context, subject string) ([]string, error) {
	normalizedSubject, err := normalizeRequiredInput("subject", subject)
	if err != nil {
		return nil, err
	}
	if err := checkContext(ctx); err != nil {
		return nil, err
	}

	roles, err := m.backend.Roles(ctx, normalizedSubject)
	if err != nil {
		return nil, fmt.Errorf("rbac: backend %s list roles for subject %q: %w", m.backendIdentity, normalizedSubject, err)
	}
	return normalizeBackendRoles(m.backendIdentity, roles)
}

func (m *manager) RolePermissions(ctx context.Context, role string) ([]Permission, error) {
	normalizedRole, err := normalizeRequiredInput("role", role)
	if err != nil {
		return nil, err
	}
	if err := checkContext(ctx); err != nil {
		return nil, err
	}

	permissions, err := m.backend.RolePermissions(ctx, normalizedRole)
	if err != nil {
		return nil, fmt.Errorf("rbac: backend %s list permissions for role %q: %w", m.backendIdentity, normalizedRole, err)
	}
	return normalizeBackendPermissions(m.backendIdentity, permissions)
}

func (m *manager) ReplaceSubjectRoles(ctx context.Context, subject string, roles []string) (bool, error) {
	normalizedSubject, err := normalizeRequiredInput("subject", subject)
	if err != nil {
		return false, err
	}
	normalizedRoles, err := normalizeRoleInputs(roles)
	if err != nil {
		return false, err
	}
	if err := checkContext(ctx); err != nil {
		return false, err
	}

	changed, err := m.backend.ReplaceSubjectRoles(ctx, normalizedSubject, normalizedRoles)
	if err != nil {
		return changed, fmt.Errorf("rbac: backend %s replace roles for subject %q: %w", m.backendIdentity, normalizedSubject, err)
	}
	return changed, nil
}

func (m *manager) ReplaceRolePermissions(ctx context.Context, role string, permissions []Permission) (bool, error) {
	normalizedRole, err := normalizeRequiredInput("role", role)
	if err != nil {
		return false, err
	}
	normalizedPermissions, err := normalizePermissionInputs(permissions)
	if err != nil {
		return false, err
	}
	if err := checkContext(ctx); err != nil {
		return false, err
	}

	changed, err := m.backend.ReplaceRolePermissions(ctx, normalizedRole, normalizedPermissions)
	if err != nil {
		return changed, fmt.Errorf("rbac: backend %s replace permissions for role %q: %w", m.backendIdentity, normalizedRole, err)
	}
	return changed, nil
}

func (m *manager) DeleteSubject(ctx context.Context, subject string) (bool, error) {
	normalizedSubject, err := normalizeRequiredInput("subject", subject)
	if err != nil {
		return false, err
	}
	if err := checkContext(ctx); err != nil {
		return false, err
	}

	changed, err := m.backend.DeleteSubject(ctx, normalizedSubject)
	if err != nil {
		return changed, fmt.Errorf("rbac: backend %s delete subject %q: %w", m.backendIdentity, normalizedSubject, err)
	}
	return changed, nil
}

func (m *manager) DeleteRole(ctx context.Context, role string) (bool, error) {
	normalizedRole, err := normalizeRequiredInput("role", role)
	if err != nil {
		return false, err
	}
	if err := checkContext(ctx); err != nil {
		return false, err
	}

	changed, err := m.backend.DeleteRole(ctx, normalizedRole)
	if err != nil {
		return changed, fmt.Errorf("rbac: backend %s delete role %q: %w", m.backendIdentity, normalizedRole, err)
	}
	return changed, nil
}

func (m *manager) isAdmin(ctx context.Context, subject string) (bool, error) {
	if err := checkContext(ctx); err != nil {
		return false, err
	}
	admin, err := m.backend.HasRole(ctx, subject, m.adminRole)
	if err != nil {
		return false, fmt.Errorf("rbac: backend %s check administrator role %q for subject %q: %w", m.backendIdentity, m.adminRole, subject, err)
	}
	return admin, nil
}

func (m *manager) authorize(ctx context.Context, subject string, permission Permission) (bool, error) {
	if err := checkContext(ctx); err != nil {
		return false, err
	}
	allowed, err := m.backend.Authorize(ctx, subject, permission)
	if err != nil {
		return false, fmt.Errorf("rbac: backend %s authorize subject %q for %q#%q: %w", m.backendIdentity, subject, permission.Object, permission.Action, err)
	}
	return allowed, nil
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: context must not be nil", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func normalizeRequiredInput(field, value string) (string, error) {
	normalized := strings.TrimSpace(value)
	if normalized == "" {
		return "", fmt.Errorf("%w: %s must not be empty", ErrInvalidInput, field)
	}
	return normalized, nil
}

func normalizePermissionInput(permission Permission) (Permission, error) {
	object, err := normalizeRequiredInput("permission object", permission.Object)
	if err != nil {
		return Permission{}, err
	}
	action := strings.TrimSpace(permission.Action)
	if action == "" {
		action = "*"
	}
	return Permission{Object: object, Action: action}, nil
}

func normalizeRoleInputs(roles []string) ([]string, error) {
	unique := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		normalized, err := normalizeRequiredInput("role", role)
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

func normalizePermissionInputs(permissions []Permission) ([]Permission, error) {
	unique := make(map[Permission]struct{}, len(permissions))
	for _, permission := range permissions {
		normalized, err := normalizePermissionInput(permission)
		if err != nil {
			return nil, err
		}
		unique[normalized] = struct{}{}
	}
	result := make([]Permission, 0, len(unique))
	for permission := range unique {
		result = append(result, permission)
	}
	sortPermissions(result)
	return result, nil
}

func normalizeBackendRoles(identity plugin.Identity, roles []string) ([]string, error) {
	unique := make(map[string]struct{}, len(roles))
	for index, role := range roles {
		normalized := strings.TrimSpace(role)
		if normalized == "" {
			return nil, fmt.Errorf("rbac: backend %s returned an empty role at index %d", identity, index)
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

func normalizeBackendPermissions(identity plugin.Identity, permissions []Permission) ([]Permission, error) {
	unique := make(map[Permission]struct{}, len(permissions))
	for index, permission := range permissions {
		object := strings.TrimSpace(permission.Object)
		if object == "" {
			return nil, fmt.Errorf("rbac: backend %s returned a permission with an empty object at index %d", identity, index)
		}
		action := strings.TrimSpace(permission.Action)
		if action == "" {
			action = "*"
		}
		unique[Permission{Object: object, Action: action}] = struct{}{}
	}
	result := make([]Permission, 0, len(unique))
	for permission := range unique {
		result = append(result, permission)
	}
	sortPermissions(result)
	return result, nil
}

func sortPermissions(permissions []Permission) {
	sort.Slice(permissions, func(i, j int) bool {
		if permissions[i].Object != permissions[j].Object {
			return permissions[i].Object < permissions[j].Object
		}
		return permissions[i].Action < permissions[j].Action
	})
}
