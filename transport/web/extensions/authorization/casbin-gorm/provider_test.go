package casbingorm

import (
	"fmt"
	"reflect"
	"testing"

	casbinlib "github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"
	gormadapter "github.com/casbin/gorm-adapter/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xbcio/xbc/config"
	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
	casbinplugin "github.com/xbcio/xbc/transport/web/extensions/authorization/casbin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

const testModel = `[request_definition]
r = sub, obj, act

[policy_definition]
p = sub, obj, act

[role_definition]
g = _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = g(r.sub, p.sub) && r.obj == p.obj && r.act == p.act
`

func TestDefinitionIsCanonicalAndUsesExpectedMetadata(t *testing.T) {
	assert.True(t, Definition() == Definition())

	plan, err := assembly.BuildPlan(assembly.PlanOptions{Bundles: []plugin.Bundle{Bundle(), Bundle()}})
	require.NoError(t, err)
	assert.Equal(t, 1, plan.DefinitionCount())
	assert.Equal(t, []plugin.Key{Key}, plan.Disabled())
	assert.Empty(t, plan.Order())
}

func TestPlannedDefinitionSelectsNamedGORMDatabaseAndExportsProvider(t *testing.T) {
	db := openSQLite(t)
	gormDefinition := plugin.Define(
		gormPluginKey,
		func(plugin.BuildContext) (*gorm.DB, error) { return db, nil },
		plugin.Options[*gorm.DB]{Instances: plugin.MultipleInstances},
	)
	environment := pluginEnvironment(t, map[string]any{
		"db_instance": "writer",
		"table":       "authorization",
		"migrate":     true,
	})
	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{plugin.BundleOf(gormDefinition), Bundle()},
		Env:     environment,
	})
	require.NoError(t, err)
	assert.Equal(t, []plugin.Identity{
		{Plugin: gormPluginKey, Instance: "writer"},
		{Plugin: Key, Instance: plugin.DefaultInstance},
	}, plan.Order())
	assert.Equal(t,
		[]plugin.Identity{{Plugin: Key, Instance: plugin.DefaultInstance}},
		plan.Contracts(reflect.TypeOf((*casbinplugin.AdapterProvider)(nil)).Elem()),
	)

	constructed, err := assembly.Construct(plan, assembly.ConstructOptions{
		ContextFactory: func(identity plugin.Identity, _ corelog.Logger) *plugin.Context {
			return plugin.NewRuntimeContext(nil, identity)
		},
	})
	require.NoError(t, err)
	instance, found := constructed.Instance(plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
	require.True(t, found)
	provider, ok := instance.Primary().(*Provider)
	require.True(t, ok)
	assert.Same(t, db, provider.db)
	assert.Equal(t, "authorization", provider.config.Table)
	_, ok = provider.Adapter().(*gormadapter.Adapter)
	assert.True(t, ok, "Adapter() must preserve *gormadapter.Adapter as its dynamic type")
	assert.False(t, db.Migrator().HasTable("authorization"), "construction must not perform DDL")
	require.NoError(t, instance.InvokeMigration())
	assert.True(t, db.Migrator().HasTable("authorization"), "only XBC's migration hook may perform DDL")
}

func TestConstructionNeverImplicitlyMigratesAndMissingTableFailsOnLoad(t *testing.T) {
	db := openSQLite(t)
	originalStatement := db.Statement
	provider, err := newProvider(db, Config{DBInstance: plugin.DefaultInstance, Table: defaultTable, Migrate: true})
	require.NoError(t, err)

	assert.Same(t, originalStatement, db.Statement, "TurnOffAutoMigrate must mutate only a session clone")
	assert.False(t, db.Migrator().HasTable(defaultTable))

	policyModel, err := model.NewModelFromString(testModel)
	require.NoError(t, err)
	err = provider.Adapter().LoadPolicy(policyModel)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no such table")
}

func TestMigrationFlagIsInertUntilXBCMigrationHook(t *testing.T) {
	db := openSQLite(t)
	context := lifecycleContext()

	disabled, err := newProvider(db, Config{DBInstance: plugin.DefaultInstance, Table: "disabled_rules"})
	require.NoError(t, err)
	require.NoError(t, disabled.migrate(context))
	assert.False(t, db.Migrator().HasTable("disabled_rules"))

	enabled, err := newProvider(db, Config{DBInstance: plugin.DefaultInstance, Table: "explicit_rules", Migrate: true})
	require.NoError(t, err)
	assert.False(t, db.Migrator().HasTable("explicit_rules"))
	require.NoError(t, enabled.migrate(context))
	assert.True(t, db.Migrator().HasTable("explicit_rules"))
	assert.True(t, sqliteObjectExists(t, db, "index", "idx_explicit_rules"), "migration must create upstream's equivalent unique index")

	var unique int
	require.NoError(t, db.Raw(`SELECT "unique" FROM pragma_index_list(?) WHERE name = ?`, "explicit_rules", "idx_explicit_rules").Scan(&unique).Error)
	assert.Equal(t, 1, unique)
}

func TestMigrationUsesConfiguredTableAndPrefix(t *testing.T) {
	db := openSQLite(t)
	provider, err := newProvider(db, Config{
		DBInstance:  plugin.DefaultInstance,
		Table:       "authorization",
		TablePrefix: "tenant_",
		Migrate:     true,
	})
	require.NoError(t, err)
	assert.False(t, db.Migrator().HasTable("tenant_authorization"))

	require.NoError(t, provider.migrate(lifecycleContext()))
	assert.True(t, db.Migrator().HasTable("tenant_authorization"))
	assert.False(t, db.Migrator().HasTable("authorization"))
	assert.True(t, sqliteObjectExists(t, db, "index", "idx_tenant_authorization"))
}

func TestConcreteAdapterPersistsPolicyAcrossProviderReconstruction(t *testing.T) {
	db := openSQLite(t)
	config := Config{DBInstance: plugin.DefaultInstance, Table: "persistent_rules", Migrate: true}
	first, err := newProvider(db, config)
	require.NoError(t, err)
	require.NoError(t, first.migrate(lifecycleContext()))

	policyModel, err := model.NewModelFromString(testModel)
	require.NoError(t, err)
	enforcer, err := casbinlib.NewSyncedEnforcer(policyModel, first.Adapter())
	require.NoError(t, err)
	concrete, ok := enforcer.GetAdapter().(*gormadapter.Adapter)
	require.True(t, ok)
	assert.Same(t, first.adapter, concrete)

	added, err := enforcer.AddPolicy("admin", "/reports", "GET")
	require.NoError(t, err)
	assert.True(t, added)
	added, err = enforcer.AddRoleForUser("alice", "admin")
	require.NoError(t, err)
	assert.True(t, added)

	second, err := newProvider(db, config)
	require.NoError(t, err)
	rebuiltModel, err := model.NewModelFromString(testModel)
	require.NoError(t, err)
	rebuilt, err := casbinlib.NewSyncedEnforcer(rebuiltModel, second.Adapter())
	require.NoError(t, err)
	allowed, err := rebuilt.Enforce("alice", "/reports", "GET")
	require.NoError(t, err)
	assert.True(t, allowed)

	require.NoError(t, db.Exec("SELECT 1").Error, "provider lifecycle must not close the shared pool")
}

func TestConcreteAdapterLegacyTransactionSmokeWithDefaultTable(t *testing.T) {
	db := openSQLite(t)
	config := Config{DBInstance: plugin.DefaultInstance, Table: defaultTable, Migrate: true}
	provider, err := newProvider(db, config)
	require.NoError(t, err)
	require.NoError(t, provider.migrate(lifecycleContext()))

	policyModel, err := model.NewModelFromString(testModel)
	require.NoError(t, err)
	enforcer, err := casbinlib.NewSyncedEnforcer(policyModel, provider.Adapter())
	require.NoError(t, err)
	adapter, ok := enforcer.GetAdapter().(*gormadapter.Adapter)
	require.True(t, ok)

	require.NoError(t, adapter.Transaction(enforcer, func(transaction casbinlib.IEnforcer) error {
		_, addErr := transaction.AddPolicy("auditor", "/audit", "GET")
		return addErr
	}))

	rebuiltModel, err := model.NewModelFromString(testModel)
	require.NoError(t, err)
	rebuilt, err := casbinlib.NewSyncedEnforcer(rebuiltModel, provider.Adapter())
	require.NoError(t, err)
	allowed, err := rebuilt.Enforce("auditor", "/audit", "GET")
	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestNewProviderRejectsNilDatabase(t *testing.T) {
	provider, err := newProvider(nil, defaultConfig())
	require.Error(t, err)
	assert.Nil(t, provider)
	assert.Contains(t, err.Error(), "database is nil")
}

func openSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	return db
}

func lifecycleContext() *plugin.Context {
	return plugin.NewRuntimeContext(nil, plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
}

func pluginEnvironment(t *testing.T, values map[string]any) *config.Environment {
	t.Helper()
	environment, err := config.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"writer": map[string]any{},
			},
			"casbin-gorm": map[string]any{
				"default": values,
			},
		},
	}, "")
	require.NoError(t, err)
	return environment
}

func sqliteObjectExists(t *testing.T, db *gorm.DB, kind, name string) bool {
	t.Helper()
	var count int64
	require.NoError(t, db.Raw(
		"SELECT count(*) FROM sqlite_master WHERE type = ? AND name = ?",
		kind,
		name,
	).Scan(&count).Error)
	return count == 1
}
