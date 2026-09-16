package gorm

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
	gormlib "gorm.io/gorm"
)

type testRuntimeHost struct {
	execution context.Context
}

func (h testRuntimeHost) ExecutionContext() context.Context { return h.execution }
func (testRuntimeHost) Logger() log.Logger                  { return nil }
func (testRuntimeHost) TrafficGate() <-chan struct{}        { return nil }
func (testRuntimeHost) SubmitTask(plugin.Identity, func(context.Context), bool) bool {
	return false
}
func (testRuntimeHost) RequestShutdown(plugin.Identity, string) bool { return false }

func lifecycleContext(ctx context.Context, instance string) *plugin.Context {
	return plugin.NewRuntimeContext(
		testRuntimeHost{execution: ctx},
		plugin.Identity{Plugin: Key, Instance: instance},
	)
}

func sqliteConfig(t *testing.T) Config {
	t.Helper()
	cfg := defaultConfig()
	cfg.Driver = "sqlite"
	cfg.DSN = "file:" + t.Name() + "?mode=memory&cache=shared"
	return cfg
}

func testEnvironment(t *testing.T, values map[string]any) *config.Environment {
	t.Helper()
	environment, err := config.NewEnvironment(values, "XBC_GORM_TEST_UNSET_")
	require.NoError(t, err)
	return environment
}

func TestDefaultConfigMatchesDocumentedValues(t *testing.T) {
	cfg := defaultConfig()

	assert.Equal(t, "mysql", cfg.Driver)
	assert.Empty(t, cfg.DSN)
	assert.Equal(t, 20, cfg.MaxOpenConn)
	assert.Equal(t, 10, cfg.MaxIdleConn)
	assert.Equal(t, time.Hour, cfg.ConnMaxLifetime)
	assert.Equal(t, 30*time.Minute, cfg.ConnMaxIdleTime)
	assert.False(t, cfg.PrepareStmt)
	assert.False(t, cfg.SkipDefaultTransaction)
}

func TestDefinitionIsCanonicalAndBundleDeduplicates(t *testing.T) {
	first := Definition()
	assert.True(t, first == Definition(), "Definition must return its package-level canonical handle")

	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle(), Bundle()},
		Env:     testEnvironment(t, nil),
	})
	require.NoError(t, err)
	assert.Equal(t, 2, plan.DefinitionCount())
	assert.Equal(t, []plugin.Key{Key, HealthKey}, plan.Disabled())
	assert.Empty(t, plan.Order(), "GORM remains disabled until plugins.gorm is configured")
}

func TestDefinitionPlansConfiguredNamedInstancesAndPrimaryContract(t *testing.T) {
	environment := testEnvironment(t, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"primary": map[string]any{
					"driver": "sqlite",
					"dsn":    "file:primary?mode=memory&cache=shared",
				},
				"readonly": map[string]any{
					"driver": "sqlite",
					"dsn":    "file:readonly?mode=memory&cache=shared",
				},
			},
		},
	})

	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle()},
		Env:     environment,
	})
	require.NoError(t, err)
	want := []plugin.Identity{
		{Plugin: Key, Instance: "primary"},
		{Plugin: Key, Instance: "readonly"},
	}
	// The readiness probe collects both databases, so it is ordered after them.
	assert.Equal(t, append(append([]plugin.Identity(nil), want...),
		plugin.Identity{Plugin: HealthKey, Instance: plugin.DefaultInstance}), plan.Order())
	assert.Equal(t, want, plan.Contracts(reflect.TypeOf((*gormlib.DB)(nil))))
}

func TestDefinitionConstructsDBAsThePrimaryValueAndConfiguresPool(t *testing.T) {
	environment := testEnvironment(t, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"readonly": map[string]any{
					"driver":                   "sqlite",
					"dsn":                      "file:constructed?mode=memory&cache=shared",
					"max_open_conn":            7,
					"max_idle_conn":            3,
					"conn_max_lifetime":        "17m",
					"conn_max_idle_time":       "4m",
					"prepare_stmt":             true,
					"skip_default_transaction": true,
				},
			},
		},
	})
	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle()},
		Env:     environment,
	})
	require.NoError(t, err)

	constructed, err := assembly.Construct(plan, assembly.ConstructOptions{
		ContextFactory: func(identity plugin.Identity, _ log.Logger) *plugin.Context {
			return plugin.NewRuntimeContext(testRuntimeHost{execution: context.Background()}, identity)
		},
	})
	require.NoError(t, err)
	instance, ok := constructed.Instance(plugin.Identity{Plugin: Key, Instance: "readonly"})
	require.True(t, ok)
	db, ok := instance.Primary().(*gormlib.DB)
	require.True(t, ok, "the factory must return *gorm.DB directly as the sole primary value")
	t.Cleanup(func() { _ = stopDatabase(db, context.Background()) })

	assert.True(t, db.PrepareStmt)
	assert.True(t, db.SkipDefaultTransaction)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	assert.Equal(t, 7, sqlDB.Stats().MaxOpenConnections)
	require.NoError(t, sqlDB.PingContext(context.Background()))
	assert.True(t, instance.HasStop())

	require.NoError(t, instance.StopBounded(context.Background(), time.Second))
	assert.Error(t, sqlDB.PingContext(context.Background()), "Stop must close the configured pool")
	require.NoError(t, instance.StopBounded(context.Background(), time.Second), "runtime repeated Stop is a no-op")
}

func TestLifecycleStopIsSafeAndIdempotentAfterPartialStartup(t *testing.T) {
	assert.NoError(t, stopDatabase(nil, nil), "Stop before factory ownership must be safe")

	db, err := openDatabase(plugin.BuildContext{}, sqliteConfig(t))
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)

	err = initializeDatabase(db, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-nil plugin context")

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	err = initializeDatabase(db, lifecycleContext(cancelled, "partial"))
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)

	require.NoError(t, stopDatabase(db, context.Background()))
	assert.Error(t, sqlDB.PingContext(context.Background()))
	require.NoError(t, stopDatabase(db, context.Background()), "a second Stop must remain a no-op")
}

func TestPrepareConfigRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{
			name: "missing dsn",
			mutate: func(cfg *Config) {
				cfg.DSN = "  "
			},
			want: "dsn is required",
		},
		{
			name: "unsupported driver",
			mutate: func(cfg *Config) {
				cfg.Driver = "oracle"
			},
			want: `unsupported driver "oracle"`,
		},
		{
			name: "max open",
			mutate: func(cfg *Config) {
				cfg.MaxOpenConn = 0
			},
			want: "max_open_conn must be at least 1",
		},
		{
			name: "negative max idle",
			mutate: func(cfg *Config) {
				cfg.MaxIdleConn = -1
			},
			want: "max_idle_conn cannot be negative",
		},
		{
			name: "max idle exceeds max open",
			mutate: func(cfg *Config) {
				cfg.MaxOpenConn = 1
				cfg.MaxIdleConn = 2
			},
			want: "cannot exceed max_open_conn",
		},
		{
			name: "negative lifetime",
			mutate: func(cfg *Config) {
				cfg.ConnMaxLifetime = -time.Second
			},
			want: "conn_max_lifetime cannot be negative",
		},
		{
			name: "negative idle time",
			mutate: func(cfg *Config) {
				cfg.ConnMaxIdleTime = -time.Second
			},
			want: "conn_max_idle_time cannot be negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := sqliteConfig(t)
			tt.mutate(&cfg)
			_, err := prepareConfig(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}

	cfg := sqliteConfig(t)
	prepared, err := prepareConfig(cfg)
	require.NoError(t, err)
	assert.Equal(t, cfg, prepared)
}

func TestDialectorSupportsAllConfiguredDrivers(t *testing.T) {
	tests := []struct {
		driver string
		dsn    string
		name   string
	}{
		{driver: "mysql", dsn: "user:pass@tcp(localhost:3306)/app", name: "mysql"},
		{driver: "postgres", dsn: "host=localhost user=app dbname=app", name: "postgres"},
		{driver: "sqlite", dsn: ":memory:", name: "sqlite"},
		{driver: "sqlserver", dsn: "sqlserver://user:pass@localhost:1433?database=app", name: "sqlserver"},
	}

	for _, tt := range tests {
		t.Run(tt.driver, func(t *testing.T) {
			dialect, err := dialector(tt.driver, tt.dsn)
			require.NoError(t, err)
			assert.Equal(t, tt.name, dialect.Name())
		})
	}
}

func TestInitializeDatabasePreservesWrappedCancellation(t *testing.T) {
	db, err := openDatabase(plugin.BuildContext{}, sqliteConfig(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = stopDatabase(db, context.Background()) })

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	err = initializeDatabase(db, lifecycleContext(cancelled, "cancelled"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}
