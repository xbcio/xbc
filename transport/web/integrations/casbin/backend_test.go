package casbin

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/casbin/casbin/v2/model"
	"github.com/casbin/casbin/v2/persist"

	"github.com/xbcio/xbc/security/rbac"
)

const wrongPolicyShapeModel = `[request_definition]
r = sub, obj, act

[policy_definition]
p = sub, obj

[role_definition]
g = _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = r.sub == p.sub && r.obj == p.obj
`

const wrongGroupingShapeModel = `[request_definition]
r = sub, obj, act

[policy_definition]
p = sub, obj, act

[role_definition]
g = _, _, dom

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = r.sub == p.sub && r.obj == p.obj && r.act == p.act
`

type backendTestAdapter struct {
	*testAdapter

	batchAddCalls       int
	batchRemoveCalls    int
	updateFilteredCalls int
	removeFilteredCalls int
	batchAddErrors      []error
	removeErrors        []error
	updateErrors        []error
	paths               []string
}

type updatableOnlyBackendAdapter struct {
	*testAdapter
}

var _ persist.UpdatableAdapter = (*updatableOnlyBackendAdapter)(nil)

func (a *updatableOnlyBackendAdapter) UpdatePolicy(string, string, []string, []string) error {
	return nil
}

func (a *updatableOnlyBackendAdapter) UpdatePolicies(string, string, [][]string, [][]string) error {
	return nil
}

func (a *updatableOnlyBackendAdapter) UpdateFilteredPolicies(string, string, [][]string, int, ...string) ([][]string, error) {
	return nil, nil
}

type batchOnlyBackendAdapter struct {
	*testAdapter
}

var _ persist.BatchAdapter = (*batchOnlyBackendAdapter)(nil)

func (a *batchOnlyBackendAdapter) AddPolicies(sec, ptype string, rules [][]string) error {
	for _, rule := range rules {
		if err := a.AddPolicy(sec, ptype, rule); err != nil {
			return err
		}
	}
	return nil
}

func (a *batchOnlyBackendAdapter) RemovePolicies(sec, ptype string, rules [][]string) error {
	for _, rule := range rules {
		if err := a.RemovePolicy(sec, ptype, rule); err != nil {
			return err
		}
	}
	return nil
}

var (
	_ persist.BatchAdapter     = (*backendTestAdapter)(nil)
	_ persist.UpdatableAdapter = (*backendTestAdapter)(nil)
)

func newBackendTestAdapter() *backendTestAdapter {
	return &backendTestAdapter{testAdapter: &testAdapter{}}
}

func (a *backendTestAdapter) AddPolicies(_ string, ptype string, rules [][]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.batchAddCalls++
	a.paths = append(a.paths, "add")
	if err := popError(&a.batchAddErrors); err != nil {
		return err
	}
	for _, rule := range rules {
		stored := append([]string{ptype}, rule...)
		if !containsRule(a.rules, stored) {
			a.rules = append(a.rules, stored)
		}
	}
	return nil
}

func (a *backendTestAdapter) RemovePolicies(_ string, ptype string, rules [][]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.batchRemoveCalls++
	a.paths = append(a.paths, "remove-many")
	for _, rule := range rules {
		a.rules = removeStoredRule(a.rules, append([]string{ptype}, rule...))
	}
	return nil
}

func (a *backendTestAdapter) UpdatePolicy(_ string, ptype string, oldRule, newRule []string) error {
	return a.UpdatePolicies("", ptype, [][]string{oldRule}, [][]string{newRule})
}

func (a *backendTestAdapter) UpdatePolicies(_ string, ptype string, oldRules, newRules [][]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, rule := range oldRules {
		a.rules = removeStoredRule(a.rules, append([]string{ptype}, rule...))
	}
	for _, rule := range newRules {
		stored := append([]string{ptype}, rule...)
		if !containsRule(a.rules, stored) {
			a.rules = append(a.rules, stored)
		}
	}
	return nil
}

func (a *backendTestAdapter) UpdateFilteredPolicies(_ string, ptype string, newRules [][]string, fieldIndex int, fieldValues ...string) ([][]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.updateFilteredCalls++
	a.paths = append(a.paths, "update-filtered")
	if err := popError(&a.updateErrors); err != nil {
		return nil, err
	}

	oldRules := make([][]string, 0)
	kept := make([][]string, 0, len(a.rules)+len(newRules))
	for _, existing := range a.rules {
		if len(existing) > 0 && existing[0] == ptype && matchesFilter(existing[1:], fieldIndex, fieldValues) {
			oldRules = append(oldRules, append([]string(nil), existing[1:]...))
			continue
		}
		kept = append(kept, existing)
	}
	for _, rule := range newRules {
		kept = append(kept, append([]string{ptype}, rule...))
	}
	a.rules = kept
	return oldRules, nil
}

func (a *backendTestAdapter) RemoveFilteredPolicy(_ string, ptype string, fieldIndex int, fieldValues ...string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.removeFilteredCalls++
	a.paths = append(a.paths, "remove-filtered")
	if err := popError(&a.removeErrors); err != nil {
		return err
	}
	kept := make([][]string, 0, len(a.rules))
	for _, existing := range a.rules {
		if len(existing) > 0 && existing[0] == ptype && matchesFilter(existing[1:], fieldIndex, fieldValues) {
			continue
		}
		kept = append(kept, existing)
	}
	a.rules = kept
	return nil
}

func (a *backendTestAdapter) snapshot() (rules [][]string, batchAdds, updates, removes int, paths []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneRules(a.rules), a.batchAddCalls, a.updateFilteredCalls, a.removeFilteredCalls, append([]string(nil), a.paths...)
}

func (a *backendTestAdapter) failBatchAdds(errs ...error) {
	a.mu.Lock()
	a.batchAddErrors = append([]error(nil), errs...)
	a.mu.Unlock()
}

func popError(errorsQueue *[]error) error {
	if len(*errorsQueue) == 0 {
		return nil
	}
	err := (*errorsQueue)[0]
	*errorsQueue = (*errorsQueue)[1:]
	return err
}

func containsRule(rules [][]string, want []string) bool {
	for _, rule := range rules {
		if equalRule(rule, want) {
			return true
		}
	}
	return false
}

func removeStoredRule(rules [][]string, want []string) [][]string {
	kept := make([][]string, 0, len(rules))
	for _, rule := range rules {
		if !equalRule(rule, want) {
			kept = append(kept, rule)
		}
	}
	return kept
}

type backendWatcher struct {
	mu       sync.Mutex
	callback func(string)
	updates  int
	errors   []error
}

var _ persist.Watcher = (*backendWatcher)(nil)

func (w *backendWatcher) SetUpdateCallback(callback func(string)) error {
	w.mu.Lock()
	w.callback = callback
	w.mu.Unlock()
	return nil
}

func (w *backendWatcher) Update() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.updates++
	return popError(&w.errors)
}

func (*backendWatcher) Close() {}

func (w *backendWatcher) failUpdates(errs ...error) {
	w.mu.Lock()
	w.errors = append([]error(nil), errs...)
	w.mu.Unlock()
}

func (w *backendWatcher) updateCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.updates
}

func compatibleProviderConfig(withWatcher bool) Config {
	cfg := providerConfig(withWatcher)
	cfg.RequestConvention = ConventionPathMethod
	return cfg
}

func startedBackendPlugin(t *testing.T, adapter persist.Adapter, watcher persist.Watcher) (*Plugin, *testHost) {
	t.Helper()
	cfg := compatibleProviderConfig(watcher != nil)
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatalf("normalizeConfig() error = %v", err)
	}
	var factory WatcherFactory
	if watcher != nil {
		factory = &testWatcherFactory{results: []watcherResult{{watcher: watcher}}}
	}
	p, err := newProviderPlugin(normalized, &testAdapterProvider{adapter: adapter}, factory, nil)
	if err != nil {
		t.Fatalf("newProviderPlugin() error = %v", err)
	}
	host := newTestHost()
	if err := p.start(testContext(host)); err != nil {
		host.close()
		t.Fatalf("start() error = %v", err)
	}
	t.Cleanup(func() {
		_ = p.Stop(context.Background())
		host.close()
	})
	return p, host
}

func TestBackendCapabilitiesUseActualModelAndAdapter(t *testing.T) {
	t.Run("compatible read-only source", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.RequestConvention = ConventionPathMethod
		p, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := p.Capabilities(), (rbac.Capabilities{RequestFields: 3, PolicyFields: 3, GroupingFields: 2}); got != want {
			t.Fatalf("Capabilities() = %+v, want %+v", got, want)
		}
		_, err = p.ReplaceRolePermissions(context.Background(), "admin", []rbac.Permission{{Object: "/reports", Action: "GET"}})
		if err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Fatalf("ReplaceRolePermissions() error = %v, want read-only", err)
		}
	})

	t.Run("external basic adapter", func(t *testing.T) {
		p, _ := startedBackendPlugin(t, &testAdapter{}, nil)
		got := p.Capabilities()
		if got.Mutable || got.FilteredPolicyReplace {
			t.Fatalf("Capabilities() = %+v, want no complete mutation capabilities", got)
		}
	})

	t.Run("external updatable-only adapter", func(t *testing.T) {
		p, _ := startedBackendPlugin(t, &updatableOnlyBackendAdapter{testAdapter: &testAdapter{}}, nil)
		got := p.Capabilities()
		if got.Mutable || !got.FilteredPolicyReplace {
			t.Fatalf("Capabilities() = %+v, want filtered replace without complete mutation support", got)
		}
	})

	t.Run("external batch-only adapter", func(t *testing.T) {
		p, _ := startedBackendPlugin(t, &batchOnlyBackendAdapter{testAdapter: &testAdapter{}}, nil)
		got := p.Capabilities()
		if !got.Mutable || got.FilteredPolicyReplace {
			t.Fatalf("Capabilities() = %+v, want mutation without filtered replace", got)
		}
	})

	t.Run("external full adapter", func(t *testing.T) {
		p, _ := startedBackendPlugin(t, newBackendTestAdapter(), nil)
		got := p.Capabilities()
		if !got.Mutable || !got.FilteredPolicyReplace {
			t.Fatalf("Capabilities() = %+v, want mutable filtered replace", got)
		}
	})
}

func TestBackendRejectsIncompatibleModelShapes(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want rbac.Capabilities
	}{
		{
			name: "two-field request and policy",
			cfg:  DefaultConfig(),
			want: rbac.Capabilities{RequestFields: 2, PolicyFields: 2, GroupingFields: 2},
		},
		{
			name: "wrong policy shape",
			cfg: Config{
				Model:             wrongPolicyShapeModel,
				RequestConvention: ConventionPathMethod,
				MissingPermission: MissingPermissionDeny,
			},
			want: rbac.Capabilities{RequestFields: 3, PolicyFields: 2, GroupingFields: 2},
		},
		{
			name: "wrong grouping shape",
			cfg: Config{
				Model:             wrongGroupingShapeModel,
				RequestConvention: ConventionPathMethod,
				MissingPermission: MissingPermissionDeny,
			},
			want: rbac.Capabilities{RequestFields: 3, PolicyFields: 3, GroupingFields: 3},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := New(tt.cfg)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if got := p.Capabilities(); got != tt.want {
				t.Fatalf("Capabilities() = %+v, want %+v", got, tt.want)
			}
			_, err = p.Authorize(context.Background(), "alice", rbac.Permission{Object: "/reports", Action: "GET"})
			if err == nil || !strings.Contains(err.Error(), "field counts") {
				t.Fatalf("Authorize() error = %v, want model shape failure", err)
			}
		})
	}
}

func TestBackendQueriesNormalizeAndSort(t *testing.T) {
	adapter := newBackendTestAdapter()
	adapter.setRules(
		[]string{"p", "editor", " /z ", " GET "},
		[]string{"p", "editor", "/a", ""},
		[]string{"p", "editor", "/z", "GET"},
		[]string{"g", "alice", " editor "},
		[]string{"g", "alice", "viewer"},
		[]string{"g", "alice", "editor"},
	)
	p, _ := startedBackendPlugin(t, adapter, nil)

	roles, err := p.Roles(context.Background(), " alice ")
	if err != nil {
		t.Fatalf("Roles() error = %v", err)
	}
	if want := []string{"editor", "viewer"}; !reflect.DeepEqual(roles, want) {
		t.Fatalf("Roles() = %v, want %v", roles, want)
	}
	hasRole, err := p.HasRole(context.Background(), " alice ", " viewer ")
	if err != nil || !hasRole {
		t.Fatalf("HasRole() = (%v, %v), want (true, nil)", hasRole, err)
	}
	permissions, err := p.RolePermissions(context.Background(), " editor ")
	if err != nil {
		t.Fatalf("RolePermissions() error = %v", err)
	}
	wantPermissions := []rbac.Permission{{Object: "/a", Action: "*"}, {Object: "/z", Action: "GET"}}
	if !reflect.DeepEqual(permissions, wantPermissions) {
		t.Fatalf("RolePermissions() = %+v, want %+v", permissions, wantPermissions)
	}

	allowed, err := p.Authorize(context.Background(), " alice ", rbac.Permission{Object: " /z ", Action: " GET "})
	if err != nil || !allowed {
		t.Fatalf("Authorize() = (%v, %v), want (true, nil)", allowed, err)
	}
}

func TestReplaceRolePermissionsUsesAddUpdateClearAndNoOpPaths(t *testing.T) {
	adapter := newBackendTestAdapter()
	watcher := &backendWatcher{}
	p, _ := startedBackendPlugin(t, adapter, watcher)
	ctx := context.Background()

	changed, err := p.ReplaceRolePermissions(ctx, " editor ", []rbac.Permission{
		{Object: " /reports ", Action: " GET "},
		{Object: "/reports", Action: "GET"},
		{Object: "/exports", Action: ""},
	})
	if err != nil || !changed {
		t.Fatalf("initial ReplaceRolePermissions() = (%v, %v), want (true, nil)", changed, err)
	}
	_, adds, updates, removes, paths := adapter.snapshot()
	if adds != 1 || updates != 0 || removes != 0 || !reflect.DeepEqual(paths, []string{"add"}) {
		t.Fatalf("initial mutation paths = %v (adds %d updates %d removes %d)", paths, adds, updates, removes)
	}

	changed, err = p.ReplaceRolePermissions(ctx, "editor", []rbac.Permission{
		{Object: "/reports", Action: "GET"},
		{Object: "/exports", Action: "*"},
	})
	if err != nil || changed {
		t.Fatalf("no-op ReplaceRolePermissions() = (%v, %v), want (false, nil)", changed, err)
	}
	_, addsAfterNoOp, updatesAfterNoOp, removesAfterNoOp, _ := adapter.snapshot()
	if addsAfterNoOp != adds || updatesAfterNoOp != updates || removesAfterNoOp != removes {
		t.Fatal("no-op replace called the adapter")
	}

	changed, err = p.ReplaceRolePermissions(ctx, "editor", []rbac.Permission{{Object: "/audit", Action: "POST"}})
	if err != nil || !changed {
		t.Fatalf("update ReplaceRolePermissions() = (%v, %v), want (true, nil)", changed, err)
	}
	_, _, updates, _, paths = adapter.snapshot()
	if updates != 1 || paths[len(paths)-1] != "update-filtered" {
		t.Fatalf("update mutation paths = %v, updates %d", paths, updates)
	}

	changed, err = p.ReplaceRolePermissions(ctx, "editor", nil)
	if err != nil || !changed {
		t.Fatalf("clear ReplaceRolePermissions() = (%v, %v), want (true, nil)", changed, err)
	}
	_, _, _, removes, paths = adapter.snapshot()
	if removes != 1 || paths[len(paths)-1] != "remove-filtered" {
		t.Fatalf("clear mutation paths = %v, removes %d", paths, removes)
	}
	permissions, err := p.RolePermissions(ctx, "editor")
	if err != nil || len(permissions) != 0 {
		t.Fatalf("RolePermissions() after clear = (%v, %v), want empty", permissions, err)
	}
	if got := watcher.updateCount(); got != 3 {
		t.Fatalf("watcher Update calls = %d, want one per changed path", got)
	}
}

func TestReplaceRolePermissionsReportsWatcherFailureAfterPersistence(t *testing.T) {
	adapter := newBackendTestAdapter()
	adapter.setRules([]string{"p", "editor", "/old", "GET"})
	watcher := &backendWatcher{}
	watcher.failUpdates(errors.New("watcher unavailable"))
	p, _ := startedBackendPlugin(t, adapter, watcher)

	changed, err := p.ReplaceRolePermissions(context.Background(), "editor", []rbac.Permission{{Object: "/new", Action: "POST"}})
	if !changed || err == nil || !strings.Contains(err.Error(), "may already be persisted") || !strings.Contains(err.Error(), "watcher unavailable") {
		t.Fatalf("ReplaceRolePermissions() = (%v, %v), want changed persistence warning", changed, err)
	}
	permissions, queryErr := p.RolePermissions(context.Background(), "editor")
	want := []rbac.Permission{{Object: "/new", Action: "POST"}}
	if queryErr != nil || !reflect.DeepEqual(permissions, want) {
		t.Fatalf("persisted permissions = (%+v, %v), want %+v", permissions, queryErr, want)
	}
}

func TestReplaceSubjectRolesRestoresPreviousSetAfterAddFailure(t *testing.T) {
	adapter := newBackendTestAdapter()
	adapter.setRules([]string{"g", "alice", "old"})
	p, _ := startedBackendPlugin(t, adapter, nil)
	adapter.failBatchAdds(errors.New("insert target failed"))

	changed, err := p.ReplaceSubjectRoles(context.Background(), "alice", []string{"new"})
	if changed || err == nil || !strings.Contains(err.Error(), "insert target failed") || !strings.Contains(err.Error(), "restoration was attempted") {
		t.Fatalf("ReplaceSubjectRoles() = (%v, %v), want restored failure", changed, err)
	}
	roles, queryErr := p.Roles(context.Background(), "alice")
	if queryErr != nil || !reflect.DeepEqual(roles, []string{"old"}) {
		t.Fatalf("Roles() after restoration = (%v, %v), want [old]", roles, queryErr)
	}
	loads, _ := adapter.counts()
	if loads != 2 {
		t.Fatalf("LoadPolicy calls = %d, want initial load plus recovery reload", loads)
	}
	rules, _, _, _, _ := adapter.snapshot()
	if !containsRule(rules, []string{"g", "alice", "old"}) || containsRule(rules, []string{"g", "alice", "new"}) {
		t.Fatalf("stored rules after restoration = %v", rules)
	}
}

func TestReplaceSubjectRolesNoOpClearAndConcurrentConvergence(t *testing.T) {
	adapter := newBackendTestAdapter()
	adapter.setRules([]string{"g", "alice", "initial"})
	p, _ := startedBackendPlugin(t, adapter, nil)
	ctx := context.Background()

	changed, err := p.ReplaceSubjectRoles(ctx, " alice ", []string{" initial ", "initial"})
	if err != nil || changed {
		t.Fatalf("no-op ReplaceSubjectRoles() = (%v, %v), want (false, nil)", changed, err)
	}
	changed, err = p.ReplaceSubjectRoles(ctx, "alice", nil)
	if err != nil || !changed {
		t.Fatalf("clear ReplaceSubjectRoles() = (%v, %v), want (true, nil)", changed, err)
	}
	changed, err = p.ReplaceSubjectRoles(ctx, "alice", []string{"seed"})
	if err != nil || !changed {
		t.Fatalf("seed ReplaceSubjectRoles() = (%v, %v), want (true, nil)", changed, err)
	}

	const replacements = 32
	targets := make([][]string, replacements)
	var wg sync.WaitGroup
	errs := make(chan error, replacements)
	for index := range replacements {
		targets[index] = []string{fmt.Sprintf("role-%02d-a", index), fmt.Sprintf("role-%02d-b", index)}
		wg.Add(1)
		go func(target []string) {
			defer wg.Done()
			changed, err := p.ReplaceSubjectRoles(ctx, "alice", target)
			if err != nil {
				errs <- err
				return
			}
			if !changed {
				errs <- errors.New("concurrent replacement unexpectedly reported no change")
			}
		}(targets[index])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent ReplaceSubjectRoles() error = %v", err)
	}

	roles, err := p.Roles(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	matchedTarget := false
	for _, target := range targets {
		sort.Strings(target)
		if reflect.DeepEqual(roles, target) {
			matchedTarget = true
			break
		}
	}
	if !matchedTarget {
		t.Fatalf("concurrent replacement converged to mixed roles %v", roles)
	}
}

func TestBackendDeleteUsesGenericCasbinManagement(t *testing.T) {
	adapter := newBackendTestAdapter()
	adapter.setRules(
		[]string{"p", "alice", "/self", "GET"},
		[]string{"p", "editor", "/reports", "GET"},
		[]string{"g", "alice", "editor"},
		[]string{"g", "editor", "base"},
	)
	p, _ := startedBackendPlugin(t, adapter, nil)

	changed, err := p.DeleteSubject(context.Background(), " alice ")
	if err != nil || !changed {
		t.Fatalf("DeleteSubject() = (%v, %v), want (true, nil)", changed, err)
	}
	changed, err = p.DeleteRole(context.Background(), " editor ")
	if err != nil || !changed {
		t.Fatalf("DeleteRole() = (%v, %v), want (true, nil)", changed, err)
	}
	rules, _, _, _, _ := adapter.snapshot()
	for _, rule := range rules {
		for _, field := range rule[1:] {
			if field == "alice" || field == "editor" {
				t.Fatalf("delete left matching stored rule %v", rule)
			}
		}
	}
}

func TestBackendChecksContextBeforeEveryOperation(t *testing.T) {
	p, _ := startedBackendPlugin(t, newBackendTestAdapter(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	permission := rbac.Permission{Object: "/reports", Action: "GET"}

	tests := []struct {
		name string
		call func() error
	}{
		{name: "authorize", call: func() error { _, err := p.Authorize(ctx, "alice", permission); return err }},
		{name: "has role", call: func() error { _, err := p.HasRole(ctx, "alice", "editor"); return err }},
		{name: "roles", call: func() error { _, err := p.Roles(ctx, "alice"); return err }},
		{name: "role permissions", call: func() error { _, err := p.RolePermissions(ctx, "editor"); return err }},
		{name: "replace subject roles", call: func() error { _, err := p.ReplaceSubjectRoles(ctx, "alice", []string{"editor"}); return err }},
		{name: "replace role permissions", call: func() error {
			_, err := p.ReplaceRolePermissions(ctx, "editor", []rbac.Permission{permission})
			return err
		}},
		{name: "delete subject", call: func() error { _, err := p.DeleteSubject(ctx, "alice"); return err }},
		{name: "delete role", call: func() error { _, err := p.DeleteRole(ctx, "editor"); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, context.Canceled) {
				t.Fatalf("operation error = %v, want context.Canceled", err)
			}
		})
	}
	if _, err := p.Authorize(nil, "alice", permission); err == nil || !strings.Contains(err.Error(), "non-nil context") {
		t.Fatalf("Authorize(nil) error = %v, want non-nil context", err)
	}
}

func TestBackendRejectsCallsAfterStop(t *testing.T) {
	p, _ := startedBackendPlugin(t, newBackendTestAdapter(), nil)
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	if _, err := p.Authorize(context.Background(), "alice", rbac.Permission{Object: "/reports", Action: "GET"}); !errors.Is(err, ErrStopped) {
		t.Fatalf("Authorize() error = %v, want ErrStopped", err)
	}
	if _, err := p.ReplaceSubjectRoles(context.Background(), "alice", []string{"editor"}); !errors.Is(err, ErrStopped) {
		t.Fatalf("ReplaceSubjectRoles() error = %v, want ErrStopped", err)
	}
}

func TestBackendDefensivelyValidatesInputs(t *testing.T) {
	p, _ := startedBackendPlugin(t, newBackendTestAdapter(), nil)
	ctx := context.Background()

	tests := []struct {
		name string
		call func() error
		want string
	}{
		{name: "empty subject", call: func() error { _, err := p.Authorize(ctx, " ", rbac.Permission{Object: "/reports"}); return err }, want: "subject"},
		{name: "empty object", call: func() error { _, err := p.Authorize(ctx, "alice", rbac.Permission{Object: " "}); return err }, want: "object"},
		{name: "empty role", call: func() error { _, err := p.HasRole(ctx, "alice", " "); return err }, want: "role"},
		{name: "empty role list item", call: func() error { _, err := p.ReplaceSubjectRoles(ctx, "alice", []string{"editor", " "}); return err }, want: "role"},
		{name: "empty permission object", call: func() error {
			_, err := p.ReplaceRolePermissions(ctx, "editor", []rbac.Permission{{Object: " "}})
			return err
		}, want: "object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("operation error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestBackendTestAdapterStillLoadsCasbinModels(t *testing.T) {
	// Compile-time and smoke coverage for the embedded adapter methods used by
	// both the existing provider tests and this bridge's richer adapter.
	var adapter persist.Adapter = newBackendTestAdapter()
	if err := adapter.LoadPolicy(model.Model{}); err != nil {
		t.Fatalf("LoadPolicy(empty model) error = %v", err)
	}
}
