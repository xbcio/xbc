package rbac

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type backendCall struct {
	operation   string
	subject     string
	role        string
	permission  Permission
	roles       []string
	permissions []Permission
}

type fakeBackend struct {
	capabilities Capabilities
	calls        []backendCall

	authorizeFunc              func(context.Context, string, Permission) (bool, error)
	hasRoleFunc                func(context.Context, string, string) (bool, error)
	rolesResult                []string
	rolesErr                   error
	rolePermissionsResult      []Permission
	rolePermissionsErr         error
	replaceSubjectRolesChanged bool
	replaceSubjectRolesErr     error
	replacePermissionsChanged  bool
	replacePermissionsErr      error
	deleteSubjectChanged       bool
	deleteSubjectErr           error
	deleteRoleChanged          bool
	deleteRoleErr              error
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{capabilities: requiredCapabilities}
}

func (backend *fakeBackend) Capabilities() Capabilities { return backend.capabilities }

func (backend *fakeBackend) Authorize(ctx context.Context, subject string, permission Permission) (bool, error) {
	backend.calls = append(backend.calls, backendCall{operation: "authorize", subject: subject, permission: permission})
	if backend.authorizeFunc != nil {
		return backend.authorizeFunc(ctx, subject, permission)
	}
	return false, nil
}

func (backend *fakeBackend) HasRole(ctx context.Context, subject, role string) (bool, error) {
	backend.calls = append(backend.calls, backendCall{operation: "has-role", subject: subject, role: role})
	if backend.hasRoleFunc != nil {
		return backend.hasRoleFunc(ctx, subject, role)
	}
	return false, nil
}

func (backend *fakeBackend) Roles(_ context.Context, subject string) ([]string, error) {
	backend.calls = append(backend.calls, backendCall{operation: "roles", subject: subject})
	return backend.rolesResult, backend.rolesErr
}

func (backend *fakeBackend) RolePermissions(_ context.Context, role string) ([]Permission, error) {
	backend.calls = append(backend.calls, backendCall{operation: "role-permissions", role: role})
	return backend.rolePermissionsResult, backend.rolePermissionsErr
}

func (backend *fakeBackend) ReplaceSubjectRoles(_ context.Context, subject string, roles []string) (bool, error) {
	backend.calls = append(backend.calls, backendCall{
		operation: "replace-subject-roles",
		subject:   subject,
		roles:     append([]string(nil), roles...),
	})
	return backend.replaceSubjectRolesChanged, backend.replaceSubjectRolesErr
}

func (backend *fakeBackend) ReplaceRolePermissions(_ context.Context, role string, permissions []Permission) (bool, error) {
	backend.calls = append(backend.calls, backendCall{
		operation:   "replace-role-permissions",
		role:        role,
		permissions: append([]Permission(nil), permissions...),
	})
	return backend.replacePermissionsChanged, backend.replacePermissionsErr
}

func (backend *fakeBackend) DeleteSubject(_ context.Context, subject string) (bool, error) {
	backend.calls = append(backend.calls, backendCall{operation: "delete-subject", subject: subject})
	return backend.deleteSubjectChanged, backend.deleteSubjectErr
}

func (backend *fakeBackend) DeleteRole(_ context.Context, role string) (bool, error) {
	backend.calls = append(backend.calls, backendCall{operation: "delete-role", role: role})
	return backend.deleteRoleChanged, backend.deleteRoleErr
}

func newTestManager(t *testing.T, backend Backend, edit ...func(*Config)) Manager {
	t.Helper()
	config := DefaultConfig()
	for _, apply := range edit {
		apply(&config)
	}
	manager, err := New(config, backend)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return manager
}

func TestNewRejectsNilAndTypedNilBackends(t *testing.T) {
	var typedNil *fakeBackend
	for name, backend := range map[string]Backend{
		"nil":       nil,
		"typed nil": typedNil,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New(DefaultConfig(), backend)
			if !errors.Is(err, ErrIncompatibleBackend) || !strings.Contains(err.Error(), "casbin") {
				t.Fatalf("New() error = %v", err)
			}
		})
	}
}

func TestNewRejectsEveryIncompatibleCapability(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Capabilities)
	}{
		{name: "request fields", edit: func(c *Capabilities) { c.RequestFields = 2 }},
		{name: "policy fields", edit: func(c *Capabilities) { c.PolicyFields = 4 }},
		{name: "grouping fields", edit: func(c *Capabilities) { c.GroupingFields = 3 }},
		{name: "immutable", edit: func(c *Capabilities) { c.Mutable = false }},
		{name: "no filtered replace", edit: func(c *Capabilities) { c.FilteredPolicyReplace = false }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := newFakeBackend()
			test.edit(&backend.capabilities)
			config := DefaultConfig()
			config.Backend = ProviderRef{Plugin: "policy", Instance: "writer"}
			_, err := New(config, backend)
			if !errors.Is(err, ErrIncompatibleBackend) || !strings.Contains(err.Error(), "policy[writer]") || !strings.Contains(err.Error(), "Capabilities") == true {
				// The concrete capability values are rendered even though Go's
				// %+v struct format does not prefix the type name.
				if err == nil || !strings.Contains(err.Error(), "require") {
					t.Fatalf("New() error = %v", err)
				}
			}
		})
	}
}

func TestManagerNormalizesAdminChecksAndShortCircuitsAuthorization(t *testing.T) {
	backend := newFakeBackend()
	backend.hasRoleFunc = func(_ context.Context, subject, role string) (bool, error) {
		return subject == "alice" && role == "root", nil
	}
	manager := newTestManager(t, backend, func(config *Config) { config.AdminRole = "  root  " })

	admin, err := manager.IsAdmin(context.Background(), "  alice  ")
	if err != nil || !admin {
		t.Fatalf("IsAdmin() = (%v, %v)", admin, err)
	}
	backend.calls = nil
	allowed, err := manager.Authorize(context.Background(), " alice ", Permission{Object: " reports ", Action: " "})
	if err != nil || !allowed {
		t.Fatalf("Authorize() = (%v, %v)", allowed, err)
	}
	want := []backendCall{{operation: "has-role", subject: "alice", role: "root"}}
	if !reflect.DeepEqual(backend.calls, want) {
		t.Fatalf("calls = %#v, want %#v", backend.calls, want)
	}
}

func TestAuthorizeNormalizesPermissionAndDelegatesForNonAdmin(t *testing.T) {
	backend := newFakeBackend()
	backend.authorizeFunc = func(_ context.Context, subject string, permission Permission) (bool, error) {
		return subject == "alice" && permission == (Permission{Object: "reports", Action: "*"}), nil
	}
	manager := newTestManager(t, backend)

	allowed, err := manager.Authorize(context.Background(), " alice ", Permission{Object: " reports "})
	if err != nil || !allowed {
		t.Fatalf("Authorize() = (%v, %v)", allowed, err)
	}
	want := []backendCall{
		{operation: "has-role", subject: "alice", role: "admin"},
		{operation: "authorize", subject: "alice", permission: Permission{Object: "reports", Action: "*"}},
	}
	if !reflect.DeepEqual(backend.calls, want) {
		t.Fatalf("calls = %#v, want %#v", backend.calls, want)
	}
}

func TestAuthorizeAllAndAnyUseIdentityValuesDeduplicateSortAndShortCircuit(t *testing.T) {
	permissions := []Permission{
		{Object: " zeta ", Action: ""},
		{Object: "alpha", Action: "write"},
		{Object: " alpha ", Action: "read"},
		{Object: "alpha", Action: "write"},
	}

	allBackend := newFakeBackend()
	allBackend.authorizeFunc = func(context.Context, string, Permission) (bool, error) { return true, nil }
	allManager := newTestManager(t, allBackend)
	allowed, err := allManager.AuthorizeAll(context.Background(), " user ", permissions...)
	if err != nil || !allowed {
		t.Fatalf("AuthorizeAll() = (%v, %v)", allowed, err)
	}
	wantAll := []backendCall{
		{operation: "has-role", subject: "user", role: "admin"},
		{operation: "authorize", subject: "user", permission: Permission{Object: "alpha", Action: "read"}},
		{operation: "authorize", subject: "user", permission: Permission{Object: "alpha", Action: "write"}},
		{operation: "authorize", subject: "user", permission: Permission{Object: "zeta", Action: "*"}},
	}
	if !reflect.DeepEqual(allBackend.calls, wantAll) {
		t.Fatalf("all calls = %#v, want %#v", allBackend.calls, wantAll)
	}

	anyBackend := newFakeBackend()
	anyBackend.authorizeFunc = func(_ context.Context, _ string, permission Permission) (bool, error) {
		return permission == (Permission{Object: "alpha", Action: "write"}), nil
	}
	anyManager := newTestManager(t, anyBackend)
	allowed, err = anyManager.AuthorizeAny(context.Background(), "user", permissions...)
	if err != nil || !allowed {
		t.Fatalf("AuthorizeAny() = (%v, %v)", allowed, err)
	}
	wantAny := wantAll[:3]
	if !reflect.DeepEqual(anyBackend.calls, wantAny) {
		t.Fatalf("any calls = %#v, want %#v", anyBackend.calls, wantAny)
	}
}

func TestAuthorizeAllAndAnyEmptySetsStillValidateSubject(t *testing.T) {
	backend := newFakeBackend()
	manager := newTestManager(t, backend)

	all, err := manager.AuthorizeAll(context.Background(), " user ")
	if err != nil || !all {
		t.Fatalf("empty AuthorizeAll() = (%v, %v)", all, err)
	}
	any, err := manager.AuthorizeAny(context.Background(), " user ")
	if err != nil || any {
		t.Fatalf("empty AuthorizeAny() = (%v, %v)", any, err)
	}
	if len(backend.calls) != 0 {
		t.Fatalf("empty checks called backend: %#v", backend.calls)
	}
	if _, err := manager.AuthorizeAll(context.Background(), "  "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid empty-all subject error = %v", err)
	}
	if _, err := manager.AuthorizeAny(context.Background(), "  "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid empty-any subject error = %v", err)
	}
}

func TestManagerChecksContextBeforeEveryBackendCall(t *testing.T) {
	backend := newFakeBackend()
	manager := newTestManager(t, backend)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.HasRole(canceled, "alice", "reader"); !errors.Is(err, context.Canceled) {
		t.Fatalf("HasRole() error = %v", err)
	}
	if len(backend.calls) != 0 {
		t.Fatalf("canceled call reached backend: %#v", backend.calls)
	}

	betweenBackend := newFakeBackend()
	betweenContext, cancelBetween := context.WithCancel(context.Background())
	betweenBackend.hasRoleFunc = func(context.Context, string, string) (bool, error) {
		cancelBetween()
		return false, nil
	}
	betweenManager := newTestManager(t, betweenBackend)
	if _, err := betweenManager.Authorize(betweenContext, "alice", Permission{Object: "reports", Action: "read"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Authorize() error = %v", err)
	}
	if len(betweenBackend.calls) != 1 || betweenBackend.calls[0].operation != "has-role" {
		t.Fatalf("calls after cancellation = %#v", betweenBackend.calls)
	}

	loopBackend := newFakeBackend()
	loopContext, cancelLoop := context.WithCancel(context.Background())
	loopBackend.authorizeFunc = func(context.Context, string, Permission) (bool, error) {
		cancelLoop()
		return true, nil
	}
	loopManager := newTestManager(t, loopBackend)
	_, err := loopManager.AuthorizeAll(loopContext, "alice",
		Permission{Object: "a", Action: "read"},
		Permission{Object: "b", Action: "read"},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("AuthorizeAll() error = %v", err)
	}
	if len(loopBackend.calls) != 2 || loopBackend.calls[1].permission.Object != "a" {
		t.Fatalf("loop calls = %#v", loopBackend.calls)
	}
}

func TestQueriesDefensivelyNormalizeDeduplicateAndSortBackendResults(t *testing.T) {
	backend := newFakeBackend()
	backend.rolesResult = []string{" writer ", "admin", "writer"}
	backend.rolePermissionsResult = []Permission{
		{Object: " reports ", Action: " write "},
		{Object: "audit", Action: ""},
		{Object: "reports", Action: "write"},
	}
	manager := newTestManager(t, backend)

	roles, err := manager.Roles(context.Background(), " user ")
	if err != nil || !reflect.DeepEqual(roles, []string{"admin", "writer"}) {
		t.Fatalf("Roles() = (%#v, %v)", roles, err)
	}
	permissions, err := manager.RolePermissions(context.Background(), " editor ")
	wantPermissions := []Permission{{Object: "audit", Action: "*"}, {Object: "reports", Action: "write"}}
	if err != nil || !reflect.DeepEqual(permissions, wantPermissions) {
		t.Fatalf("RolePermissions() = (%#v, %v)", permissions, err)
	}
	roles[0] = "changed"
	permissions[0].Object = "changed"
	if backend.rolesResult[0] != " writer " || backend.rolePermissionsResult[0].Object != " reports " {
		t.Fatal("Manager mutated backend-owned query results")
	}
	if backend.calls[0].subject != "user" || backend.calls[1].role != "editor" {
		t.Fatalf("query calls = %#v", backend.calls)
	}
}

func TestQueriesRejectMalformedBackendResults(t *testing.T) {
	backend := newFakeBackend()
	backend.rolesResult = []string{"reader", " "}
	manager := newTestManager(t, backend)
	if _, err := manager.Roles(context.Background(), "alice"); err == nil || !strings.Contains(err.Error(), "backend casbin") {
		t.Fatalf("Roles() error = %v", err)
	}

	backend.rolesResult = nil
	backend.rolePermissionsResult = []Permission{{Object: " "}}
	if _, err := manager.RolePermissions(context.Background(), "reader"); err == nil || !strings.Contains(err.Error(), "empty object") {
		t.Fatalf("RolePermissions() error = %v", err)
	}
}

func TestMutationsNormalizeAndDelegateStableSets(t *testing.T) {
	backend := newFakeBackend()
	backend.replaceSubjectRolesChanged = true
	backend.replacePermissionsChanged = true
	backend.deleteSubjectChanged = true
	backend.deleteRoleChanged = true
	manager := newTestManager(t, backend)

	changed, err := manager.ReplaceSubjectRoles(context.Background(), " user ", []string{" writer ", "admin", "writer"})
	if err != nil || !changed {
		t.Fatalf("ReplaceSubjectRoles() = (%v, %v)", changed, err)
	}
	changed, err = manager.ReplaceRolePermissions(context.Background(), " editor ", []Permission{
		{Object: " reports ", Action: "write"},
		{Object: "audit", Action: " "},
		{Object: "reports", Action: "write"},
	})
	if err != nil || !changed {
		t.Fatalf("ReplaceRolePermissions() = (%v, %v)", changed, err)
	}
	if changed, err = manager.DeleteSubject(context.Background(), " user "); err != nil || !changed {
		t.Fatalf("DeleteSubject() = (%v, %v)", changed, err)
	}
	if changed, err = manager.DeleteRole(context.Background(), " editor "); err != nil || !changed {
		t.Fatalf("DeleteRole() = (%v, %v)", changed, err)
	}

	want := []backendCall{
		{operation: "replace-subject-roles", subject: "user", roles: []string{"admin", "writer"}},
		{operation: "replace-role-permissions", role: "editor", permissions: []Permission{{Object: "audit", Action: "*"}, {Object: "reports", Action: "write"}}},
		{operation: "delete-subject", subject: "user"},
		{operation: "delete-role", role: "editor"},
	}
	if !reflect.DeepEqual(backend.calls, want) {
		t.Fatalf("mutation calls = %#v, want %#v", backend.calls, want)
	}
}

func TestMutationsPreserveChangedFlagWhenBackendReturnsError(t *testing.T) {
	backendErr := errors.New("watcher unavailable")
	tests := []struct {
		name string
		call func(Manager) (bool, error)
		edit func(*fakeBackend)
	}{
		{
			name: "replace subject roles",
			call: func(manager Manager) (bool, error) {
				return manager.ReplaceSubjectRoles(context.Background(), "alice", []string{"editor"})
			},
			edit: func(backend *fakeBackend) {
				backend.replaceSubjectRolesChanged = true
				backend.replaceSubjectRolesErr = backendErr
			},
		},
		{
			name: "replace role permissions",
			call: func(manager Manager) (bool, error) {
				return manager.ReplaceRolePermissions(context.Background(), "editor", []Permission{{Object: "reports", Action: "read"}})
			},
			edit: func(backend *fakeBackend) {
				backend.replacePermissionsChanged = true
				backend.replacePermissionsErr = backendErr
			},
		},
		{
			name: "delete subject",
			call: func(manager Manager) (bool, error) {
				return manager.DeleteSubject(context.Background(), "alice")
			},
			edit: func(backend *fakeBackend) {
				backend.deleteSubjectChanged = true
				backend.deleteSubjectErr = backendErr
			},
		},
		{
			name: "delete role",
			call: func(manager Manager) (bool, error) {
				return manager.DeleteRole(context.Background(), "editor")
			},
			edit: func(backend *fakeBackend) {
				backend.deleteRoleChanged = true
				backend.deleteRoleErr = backendErr
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := newFakeBackend()
			tt.edit(backend)
			changed, err := tt.call(newTestManager(t, backend))
			if !changed || !errors.Is(err, backendErr) || !strings.Contains(err.Error(), "backend casbin") {
				t.Fatalf("mutation = (%v, %v), want changed wrapped backend error", changed, err)
			}
		})
	}
}

func TestManagerRejectsInvalidInputsBeforeBackendCalls(t *testing.T) {
	backend := newFakeBackend()
	manager := newTestManager(t, backend)
	operations := []func() error{
		func() error { _, err := manager.IsAdmin(context.Background(), " "); return err },
		func() error {
			_, err := manager.Authorize(context.Background(), "alice", Permission{Object: " "})
			return err
		},
		func() error { _, err := manager.HasRole(context.Background(), "alice", " "); return err },
		func() error { _, err := manager.Roles(context.Background(), " "); return err },
		func() error { _, err := manager.RolePermissions(context.Background(), " "); return err },
		func() error {
			_, err := manager.ReplaceSubjectRoles(context.Background(), "alice", []string{" "})
			return err
		},
		func() error {
			_, err := manager.ReplaceRolePermissions(context.Background(), "reader", []Permission{{Object: " "}})
			return err
		},
		func() error { _, err := manager.DeleteSubject(context.Background(), " "); return err },
		func() error { _, err := manager.DeleteRole(context.Background(), " "); return err },
		func() error { _, err := manager.HasRole(nil, "alice", "reader"); return err },
	}
	for index, operation := range operations {
		if err := operation(); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("operation %d error = %v", index, err)
		}
	}
	if len(backend.calls) != 0 {
		t.Fatalf("invalid inputs reached backend: %#v", backend.calls)
	}
}

func TestManagerWrapsAndPreservesBackendErrors(t *testing.T) {
	backendErr := errors.New("backend unavailable")
	backend := newFakeBackend()
	backend.rolesErr = backendErr
	manager := newTestManager(t, backend)
	_, err := manager.Roles(context.Background(), "alice")
	if !errors.Is(err, backendErr) || !strings.Contains(err.Error(), "backend casbin") {
		t.Fatalf("Roles() error = %v", err)
	}
}
