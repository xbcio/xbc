package rbac

import "context"

// Permission identifies one business operation on an object. An empty Action
// is normalized to "*" by Manager implementations.
type Permission struct {
	Object string
	Action string
}

// Capabilities describes the policy shape and mutation guarantees a Backend
// provides. Mutable means the backend can persist the Manager's complete
// mutation set, including batch additions. The built-in Manager requires a
// 3/3/2 request, policy, and grouping shape together with persistent mutation
// and filtered policy replacement.
type Capabilities struct {
	RequestFields         int
	PolicyFields          int
	GroupingFields        int
	Mutable               bool
	FilteredPolicyReplace bool
}

// Backend is the integration-facing contract beneath the business RBAC
// Manager. Implementations own policy persistence, synchronization, and any
// serialization needed to make replace operations safe for one backend
// instance.
type Backend interface {
	Capabilities() Capabilities
	Authorize(context.Context, string, Permission) (bool, error)
	HasRole(context.Context, string, string) (bool, error)
	Roles(context.Context, string) ([]string, error)
	RolePermissions(context.Context, string) ([]Permission, error)
	ReplaceSubjectRoles(context.Context, string, []string) (bool, error)
	ReplaceRolePermissions(context.Context, string, []Permission) (bool, error)
	DeleteSubject(context.Context, string) (bool, error)
	DeleteRole(context.Context, string) (bool, error)
}

// Manager is the application-facing RBAC contract. It normalizes business
// inputs, applies the configured administrator role, and delegates durable
// policy operations to one exact Backend instance.
type Manager interface {
	IsAdmin(ctx context.Context, subject string) (bool, error)
	Authorize(ctx context.Context, subject string, permission Permission) (bool, error)
	AuthorizeAll(ctx context.Context, subject string, permissions ...Permission) (bool, error)
	AuthorizeAny(ctx context.Context, subject string, permissions ...Permission) (bool, error)

	HasRole(ctx context.Context, subject, role string) (bool, error)
	Roles(ctx context.Context, subject string) ([]string, error)
	RolePermissions(ctx context.Context, role string) ([]Permission, error)

	ReplaceSubjectRoles(ctx context.Context, subject string, roles []string) (bool, error)
	ReplaceRolePermissions(ctx context.Context, role string, permissions []Permission) (bool, error)
	DeleteSubject(ctx context.Context, subject string) (bool, error)
	DeleteRole(ctx context.Context, role string) (bool, error)
}
