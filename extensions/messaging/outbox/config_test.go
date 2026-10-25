package outbox

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
	"gorm.io/gorm"
)

func retentionPlan(t *testing.T, db *gorm.DB, outbox map[string]any) (*assembly.Plan, error) {
	t.Helper()
	gormDefinition := plugin.Define(
		gormPluginKey,
		func(plugin.BuildContext) (*gorm.DB, error) { return db, nil },
	)
	return assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{plugin.BundleOf(gormDefinition), Bundle()},
		Env:     outboxEnvironment(t, nil, outbox),
	})
}

// TestRetentionConfigurationThroughTheFramework pins the unset-versus-zero
// rule on the real binding path: an absent key keeps the pre-filled default,
// while an explicit zero overwrites it and survives normalization.
func TestRetentionConfigurationThroughTheFramework(t *testing.T) {
	db := openPluginTestDB(t)
	defaults := RetentionConfig{Published: 168 * time.Hour, Interval: time.Hour, BatchSize: 1000}
	cases := []struct {
		name      string
		retention any
		want      RetentionConfig
	}{
		{"section absent", nil, defaults},
		{"empty section", map[string]any{}, defaults},
		{"published zero disables cleanup", map[string]any{"published": 0},
			RetentionConfig{Interval: time.Hour, BatchSize: 1000}},
		{"published zero duration string", map[string]any{"published": "0s"},
			RetentionConfig{Interval: time.Hour, BatchSize: 1000}},
		{"dead alone keeps the published default", map[string]any{"dead": "720h"},
			RetentionConfig{Published: 168 * time.Hour, Dead: 720 * time.Hour, Interval: time.Hour, BatchSize: 1000}},
		{"explicit dead zero", map[string]any{"dead": 0}, defaults},
		{"all keys", map[string]any{"published": "72h", "dead": "240h", "interval": "30m", "batch_size": 50},
			RetentionConfig{Published: 72 * time.Hour, Dead: 240 * time.Hour, Interval: 30 * time.Minute, BatchSize: 50}},
		{"zero interval and batch_size mean the default", map[string]any{"interval": 0, "batch_size": 0}, defaults},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			section := map[string]any{"db_instance": "default"}
			if tc.retention != nil {
				section["retention"] = tc.retention
			}
			plan, err := retentionPlan(t, db, section)
			require.NoError(t, err)
			constructed, err := assembly.Construct(plan, assembly.ConstructOptions{})
			require.NoError(t, err)
			instance, found := constructed.Instance(plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
			require.True(t, found)
			service := instance.Primary().(*Service)
			assert.Equal(t, tc.want, service.config.Retention)
		})
	}
}

func TestRetentionConfigurationRejectsNegativeValuesThroughTheFramework(t *testing.T) {
	db := openPluginTestDB(t)
	for name, retention := range map[string]map[string]any{
		"published": {"published": "-1h"},
		"dead":      {"dead": "-1s"},
		"interval":  {"interval": "-1m"},
		"batch":     {"batch_size": -5},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := retentionPlan(t, db, map[string]any{"db_instance": "default", "retention": retention})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "retention")
		})
	}
}

func TestPrepareConfigValidatesRetention(t *testing.T) {
	valid := defaultConfig()
	require.NoError(t, valid.Validate())

	for name, mutate := range map[string]func(*Config){
		"negative published": func(c *Config) { c.Retention.Published = -time.Second },
		"negative dead":      func(c *Config) { c.Retention.Dead = -time.Second },
		"negative interval":  func(c *Config) { c.Retention.Interval = -time.Second },
		"negative batch":     func(c *Config) { c.Retention.BatchSize = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			config := defaultConfig()
			mutate(&config)
			_, err := prepareConfig(config)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "retention")
			assert.Error(t, config.Validate())
		})
	}

	t.Run("zero interval and batch_size take the default like every other field", func(t *testing.T) {
		config := defaultConfig()
		config.Retention.Interval = 0
		config.Retention.BatchSize = 0
		prepared, err := prepareConfig(config)
		require.NoError(t, err)
		assert.Equal(t, time.Hour, prepared.Retention.Interval)
		assert.Equal(t, 1000, prepared.Retention.BatchSize)
	})

	t.Run("explicit zero retention is preserved", func(t *testing.T) {
		config := defaultConfig()
		config.Retention.Published = 0
		config.Retention.Dead = 0
		prepared, err := prepareConfig(config)
		require.NoError(t, err)
		assert.Zero(t, prepared.Retention.Published)
		assert.Zero(t, prepared.Retention.Dead)
		assert.False(t, prepared.Retention.enabled())
	})
}

func TestRetentionEnabledNeedsAtLeastOneNonZeroRetention(t *testing.T) {
	assert.False(t, RetentionConfig{}.enabled())
	assert.True(t, RetentionConfig{Published: time.Hour}.enabled())
	assert.True(t, RetentionConfig{Dead: time.Hour}.enabled())
}

func TestMigrateCreatesTheRetentionIndex(t *testing.T) {
	db, store := sqliteStore(t)
	var columns []string
	require.NoError(t, db.Raw(
		"SELECT name FROM pragma_index_info('idx_outbox_retention') ORDER BY seqno",
	).Scan(&columns).Error)
	assert.Equal(t, []string{"status", "updated_at"}, columns)
	require.NoError(t, store.Migrate(t.Context()), "migrating twice must be harmless")
}
