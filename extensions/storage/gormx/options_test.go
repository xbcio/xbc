package gormx

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gormlogger "gorm.io/gorm/logger"
)

func TestWithOptionsSetTheirConfigFields(t *testing.T) {
	logger := &recordingLogger{}
	lifetime := 17 * time.Minute
	idleTime := 4 * time.Minute

	tests := []struct {
		name   string
		option Option
		assert func(*testing.T, Config)
	}{
		{
			name:   "driver",
			option: WithDriver("postgres"),
			assert: func(t *testing.T, cfg Config) { assert.Equal(t, "postgres", cfg.Driver) },
		},
		{
			name:   "dsn",
			option: WithDSN("custom-dsn"),
			assert: func(t *testing.T, cfg Config) { assert.Equal(t, "custom-dsn", cfg.DSN) },
		},
		{
			name:   "max open conn",
			option: WithMaxOpenConn(7),
			assert: func(t *testing.T, cfg Config) { assert.Equal(t, 7, cfg.MaxOpenConn) },
		},
		{
			name:   "max idle conn",
			option: WithMaxIdleConn(3),
			assert: func(t *testing.T, cfg Config) { assert.Equal(t, 3, cfg.MaxIdleConn) },
		},
		{
			name:   "conn max lifetime",
			option: WithConnMaxLifetime(lifetime),
			assert: func(t *testing.T, cfg Config) { assert.Equal(t, lifetime, cfg.ConnMaxLifetime) },
		},
		{
			name:   "conn max idle time",
			option: WithConnMaxIdleTime(idleTime),
			assert: func(t *testing.T, cfg Config) { assert.Equal(t, idleTime, cfg.ConnMaxIdleTime) },
		},
		{
			name:   "prepare stmt",
			option: WithPrepareStmt(true),
			assert: func(t *testing.T, cfg Config) { assert.True(t, cfg.PrepareStmt) },
		},
		{
			name:   "skip default transaction",
			option: WithSkipDefaultTransaction(true),
			assert: func(t *testing.T, cfg Config) { assert.True(t, cfg.SkipDefaultTransaction) },
		},
		{
			name:   "translate error",
			option: WithTranslateError(true),
			assert: func(t *testing.T, cfg Config) { assert.True(t, cfg.TranslateError) },
		},
		{
			name:   "logger",
			option: WithLogger(logger),
			assert: func(t *testing.T, cfg Config) { assert.Same(t, logger, cfg.Logger) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.assert(t, applyOptions(DefaultConfig(), []Option{tt.option}))
		})
	}
}

func TestOptionsAreAppliedInOrder(t *testing.T) {
	cfg := applyOptions(DefaultConfig(), []Option{
		WithDriver("postgres"),
		WithDriver("sqlite"),
	})
	assert.Equal(t, "sqlite", cfg.Driver)
}

func TestOpenIgnoresNilOptions(t *testing.T) {
	db, err := Open(sqliteConfig(t), nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = Close(db) })

	require.NoError(t, db.Exec("CREATE TABLE nil_option_probe (id INTEGER PRIMARY KEY)").Error)
}

func TestOpenAppliesOptionsOverTheSuppliedConfig(t *testing.T) {
	logger := &recordingLogger{}
	cfg := sqliteConfig(t)
	cfg.MaxOpenConn = 3
	cfg.MaxIdleConn = 2

	db, err := Open(cfg,
		nil,
		WithDSN("file:"+t.Name()+"-override?mode=memory&cache=shared"),
		WithMaxOpenConn(5),
		WithMaxIdleConn(4),
		WithPrepareStmt(true),
		WithSkipDefaultTransaction(true),
		WithTranslateError(true),
		WithLogger(logger),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = Close(db) })

	assert.True(t, db.PrepareStmt)
	assert.True(t, db.SkipDefaultTransaction)
	assert.True(t, db.Config.TranslateError)
	assert.Same(t, logger, db.Config.Logger)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	assert.Equal(t, 5, sqlDB.Stats().MaxOpenConnections)

	assert.Equal(t, 3, cfg.MaxOpenConn, "Open must not mutate the caller's config")
	assert.Equal(t, "file:"+t.Name()+"?mode=memory&cache=shared", cfg.DSN)
	assert.Nil(t, cfg.Logger)
}

func TestNewRequiresADSNAfterDefaults(t *testing.T) {
	_, err := New()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dsn is required")

	_, err = New(WithDriver("sqlite"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dsn is required")
}

func TestNewStartsFromDefaultConfig(t *testing.T) {
	db, err := New(
		nil,
		WithDriver("sqlite"),
		WithDSN("file:"+t.Name()+"?mode=memory&cache=shared"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = Close(db) })

	assert.False(t, db.PrepareStmt)
	assert.False(t, db.SkipDefaultTransaction)
	assert.False(t, db.Config.TranslateError)
	assert.Same(t, gormlogger.Default, db.Config.Logger)

	sqlDB, err := db.DB()
	require.NoError(t, err)
	assert.Equal(t, defaultMaxOpenConn, sqlDB.Stats().MaxOpenConnections)
}

func TestNewAppliesOptionsOverDefaults(t *testing.T) {
	logger := &recordingLogger{}
	db, err := New(
		WithDriver("sqlite"),
		WithDSN("file:"+t.Name()+"?mode=memory&cache=shared"),
		WithMaxOpenConn(6),
		WithMaxIdleConn(2),
		WithPrepareStmt(true),
		WithSkipDefaultTransaction(true),
		WithTranslateError(true),
		WithLogger(logger),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = Close(db) })

	assert.True(t, db.PrepareStmt)
	assert.True(t, db.SkipDefaultTransaction)
	assert.True(t, db.Config.TranslateError)
	assert.Same(t, logger, db.Config.Logger)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	assert.Equal(t, 6, sqlDB.Stats().MaxOpenConnections)
}
