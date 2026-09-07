package examples_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	casbinlib "github.com/casbin/casbin/v2"
	gormadapter "github.com/casbin/gorm-adapter/v3"
	gormlib "gorm.io/gorm"

	"github.com/xbcio/xbc"
	gormplugin "github.com/xbcio/xbc/integrations/gorm"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/security/rbac"
	casbinplugin "github.com/xbcio/xbc/transport/web/integrations/casbin"
	casbingorm "github.com/xbcio/xbc/transport/web/integrations/casbin-gorm"
	casbinredis "github.com/xbcio/xbc/transport/web/integrations/casbin-redis"
)

const stackWait = 5 * time.Second

type stackHandles struct {
	enforcer *casbinlib.SyncedEnforcer
	manager  rbac.Manager
	database *gormlib.DB
}

type stackProbe struct {
	handles stackHandles
	ready   chan<- stackHandles
}

func (probe *stackProbe) start(ctx *plugin.Context) error {
	if !ctx.Go(func(taskCtx context.Context) { <-taskCtx.Done() }) {
		return fmt.Errorf("casbin stack probe: runtime rejected liveness task")
	}
	probe.ready <- probe.handles
	return nil
}

func stackProbeBundle(ready chan<- stackHandles) plugin.Bundle {
	enforcerProvider := plugin.RefTo[casbinplugin.EnforcerProvider](casbinplugin.Key)
	manager := plugin.RefTo[rbac.Manager](rbac.Key)
	database := plugin.RefToInstance[*gormlib.DB](gormplugin.Key, plugin.DefaultInstance)
	definition := plugin.Define(
		"casbin-stack-probe",
		func(ctx plugin.BuildContext) (*stackProbe, error) {
			provider := enforcerProvider.Get(ctx).Value
			enforcer, active := provider.Enforcer()
			if !active || enforcer == nil {
				return nil, fmt.Errorf("casbin stack probe: enforcer is not active")
			}
			return &stackProbe{
				handles: stackHandles{
					enforcer: enforcer,
					manager:  manager.Get(ctx).Value,
					database: database.Get(ctx).Value,
				},
				ready: ready,
			}, nil
		},
		plugin.Options[*stackProbe]{
			Inputs: plugin.Inputs(enforcerProvider, manager, database),
			Lifecycle: plugin.Lifecycle[*stackProbe]{
				Start: (*stackProbe).start,
			},
		},
	)
	return plugin.BundleOf(definition)
}

type executionResult struct {
	code int
	err  error
}

type runningStack struct {
	handles  stackHandles
	cancel   context.CancelFunc
	done     <-chan executionResult
	stopOnce sync.Once
}

func startStack(t *testing.T, configPath string, migrate bool) *runningStack {
	t.Helper()
	ready := make(chan stackHandles, 1)
	app, err := xbc.New(xbc.WithBundles(
		gormplugin.Bundle(),
		casbingorm.Bundle(),
		casbinredis.Bundle(),
		casbinplugin.Bundle(),
		rbac.Bundle(),
		stackProbeBundle(ready),
	))
	if err != nil {
		t.Fatalf("xbc.New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan executionResult, 1)
	args := []string{"--config", configPath}
	if migrate {
		args = append(args, "--migrate")
	}
	go func() {
		code, executeErr := app.Execute(ctx, args)
		done <- executionResult{code: code, err: executeErr}
	}()

	var handles stackHandles
	select {
	case handles = <-ready:
	case result := <-done:
		cancel()
		t.Fatalf("stack exited before Start: code=%d error=%v", result.code, result.err)
	case <-time.After(stackWait):
		cancel()
		t.Fatal("timed out waiting for Casbin stack to start")
	}
	if handles.enforcer == nil || handles.manager == nil || handles.database == nil {
		cancel()
		t.Fatalf("stack handles = %+v, want non-nil enforcer, manager, and database", handles)
	}

	running := &runningStack{handles: handles, cancel: cancel, done: done}
	t.Cleanup(func() { running.stop(t) })
	return running
}

func (stack *runningStack) stop(t *testing.T) {
	t.Helper()
	stack.stopOnce.Do(func() {
		stack.cancel()
		select {
		case result := <-stack.done:
			if result.code != 0 || result.err != nil {
				t.Errorf("stack shutdown: code=%d error=%v", result.code, result.err)
			}
		case <-time.After(stackWait):
			t.Error("timed out waiting for Casbin stack shutdown")
		}
	})
}

func TestGORMCasbinRedisRBACComposition(t *testing.T) {
	redisServer := miniredis.RunT(t)
	channel := "/xbc-casbin-composition"
	databasePath := filepath.Join(t.TempDir(), "authorization.db")
	configPath := writeStackConfig(t, databasePath, redisServer.Addr(), channel)

	// A fresh custom table does not exist before this boot. Reaching probe Start
	// proves XBC invoked casbin-gorm Migrate before Casbin's first LoadPolicy.
	first := startStack(t, configPath, true)
	if !first.handles.database.Migrator().HasTable("sys_casbin_rule") {
		t.Fatal("custom policy table was not created during the migration stage")
	}
	if _, ok := first.handles.enforcer.GetAdapter().(*gormadapter.Adapter); !ok {
		t.Fatalf("GetAdapter() = %T, want *gormadapter.Adapter", first.handles.enforcer.GetAdapter())
	}

	// This boot deliberately skips migration and succeeds against the schema
	// created above, while opening an independent watcher/backend pair.
	second := startStack(t, configPath, false)
	awaitStack(t, "both Redis watcher subscriptions", func() bool {
		return redisServer.PubSubNumSub(channel)[channel] == 2
	})

	ctx := context.Background()
	changed, err := first.handles.manager.ReplaceRolePermissions(ctx, " r8 ", []rbac.Permission{
		{Object: " settings.role.read ", Action: " read "},
		{Object: "settings.role.read", Action: "read"},
	})
	if err != nil || !changed {
		t.Fatalf("ReplaceRolePermissions(initial) = (%v, %v)", changed, err)
	}
	changed, err = first.handles.manager.ReplaceSubjectRoles(ctx, " u17 ", []string{" r8 ", "r8"})
	if err != nil || !changed {
		t.Fatalf("ReplaceSubjectRoles(initial) = (%v, %v)", changed, err)
	}
	awaitStack(t, "initial RBAC synchronization", func() bool {
		allowed, enforceErr := second.handles.manager.Authorize(ctx, "u17", rbac.Permission{Object: "settings.role.read", Action: "read"})
		return enforceErr == nil && allowed
	})

	// Both old and new permission sets are non-empty, so the Casbin bridge uses
	// filtered policy update. An empty action is normalized to SAS's wildcard.
	changed, err = first.handles.manager.ReplaceRolePermissions(ctx, "r8", []rbac.Permission{{Object: "settings.role.manage"}})
	if err != nil || !changed {
		t.Fatalf("ReplaceRolePermissions(update) = (%v, %v)", changed, err)
	}
	awaitStack(t, "filtered permission replace synchronization", func() bool {
		oldAllowed, oldErr := second.handles.manager.Authorize(ctx, "u17", rbac.Permission{Object: "settings.role.read", Action: "read"})
		newAllowed, newErr := second.handles.manager.Authorize(ctx, "u17", rbac.Permission{Object: "settings.role.manage", Action: "write"})
		permissions, permissionsErr := second.handles.manager.RolePermissions(ctx, "r8")
		return oldErr == nil && !oldAllowed && newErr == nil && newAllowed && permissionsErr == nil &&
			slices.Equal(permissions, []rbac.Permission{{Object: "settings.role.manage", Action: "*"}})
	})

	changed, err = first.handles.manager.ReplaceRolePermissions(ctx, "r9", []rbac.Permission{{Object: "settings.audit", Action: "read"}})
	if err != nil || !changed {
		t.Fatalf("ReplaceRolePermissions(second role) = (%v, %v)", changed, err)
	}
	changed, err = first.handles.manager.ReplaceSubjectRoles(ctx, "u17", []string{"r9"})
	if err != nil || !changed {
		t.Fatalf("ReplaceSubjectRoles(update) = (%v, %v)", changed, err)
	}
	awaitStack(t, "subject role replace synchronization", func() bool {
		roles, rolesErr := second.handles.manager.Roles(ctx, "u17")
		oldAllowed, oldErr := second.handles.manager.Authorize(ctx, "u17", rbac.Permission{Object: "settings.role.manage", Action: "write"})
		newAllowed, newErr := second.handles.manager.Authorize(ctx, "u17", rbac.Permission{Object: "settings.audit", Action: "read"})
		return rolesErr == nil && slices.Equal(roles, []string{"r9"}) && oldErr == nil && !oldAllowed && newErr == nil && newAllowed
	})

	changed, err = first.handles.manager.ReplaceSubjectRoles(ctx, "u17", []string{" r9 ", "r9"})
	if err != nil || changed {
		t.Fatalf("ReplaceSubjectRoles(no-op) = (%v, %v), want (false, nil)", changed, err)
	}

	// Close every writer/watcher and reconstruct a third backend. It cannot
	// receive an old Pub/Sub event, so authorization proves all replacements
	// were persisted to the custom GORM table.
	first.stop(t)
	second.stop(t)
	third := startStack(t, configPath, false)
	roles, err := third.handles.manager.Roles(ctx, "u17")
	if err != nil || !slices.Equal(roles, []string{"r9"}) {
		t.Fatalf("reconstructed Roles() = (%v, %v), want [r9]", roles, err)
	}
	allowed, err := third.handles.manager.Authorize(ctx, "u17", rbac.Permission{Object: "settings.audit", Action: "read"})
	if err != nil || !allowed {
		t.Fatalf("reconstructed Authorize() = (%v, %v), want persisted authorization", allowed, err)
	}
	var rows int64
	if err := third.handles.database.Table("sys_casbin_rule").Count(&rows).Error; err != nil {
		t.Fatalf("count custom policy rows: %v", err)
	}
	if rows != 3 {
		t.Fatalf("custom policy row count = %d, want 3", rows)
	}
	third.stop(t)
}

func writeStackConfig(t *testing.T, databasePath, redisAddress, channel string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application.yml")
	dsn := "file:" + databasePath + "?_busy_timeout=5000&_journal_mode=WAL"
	contents := fmt.Sprintf(`xbc:
  shutdown_timeout: 5s
log:
  console:
    enabled: false
plugins:
  gorm:
    default:
      driver: sqlite
      dsn: %q
      max_open_conn: 1
      max_idle_conn: 1
  casbin-gorm:
    default:
      db_instance: default
      table: sys_casbin_rule
      migrate: true
  casbin-redis:
    default:
      mode: standalone
      addrs: [%q]
      channel: %q
      ignore_self: true
  casbin:
    model: |
      [request_definition]
      r = sub, obj, act

      [policy_definition]
      p = sub, obj, act

      [role_definition]
      g = _, _

      [policy_effect]
      e = some(where (p.eft == allow))

      [matchers]
      m = g(r.sub, p.sub) && r.obj == p.obj && (p.act == "*" || r.act == p.act)
    request_convention: path_method
    adapter:
      plugin: casbin-gorm
      instance: default
    watcher:
      plugin: casbin-redis
      instance: default
  rbac:
    backend:
      plugin: casbin
      instance: default
    admin_role: admin
`, dsn, redisAddress, channel)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write stack config: %v", err)
	}
	return path
}

func awaitStack(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(stackWait)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}
