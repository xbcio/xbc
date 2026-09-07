package gorm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/xbcio/xbc/plugin"
	gormlib "gorm.io/gorm"
)

// Key is the stable configuration and dependency identity of the GORM plugin.
const Key plugin.Key = "gorm"

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: defaultConfig,
		Prepare:  prepareConfig,
	},
	openDatabase,
	plugin.Options[*gormlib.DB]{
		Instances:  plugin.MultipleInstances,
		Activation: plugin.WhenConfigured("plugins.gorm"),
		Inputs:     plugin.Inputs(),
		Exports:    plugin.Contracts[*gormlib.DB](),
		Lifecycle: plugin.Lifecycle[*gormlib.DB]{
			Init: initializeDatabase,
			Stop: stopDatabase,
		},
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns GORM's canonical immutable declaration handle. Every call
// returns the same handle and does not mutate process-global composition.
func Definition() plugin.Definition { return definition }

// Bundle returns GORM's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }

// prepareConfig performs semantic validation after XBC has applied defaults,
// bound one named instance, and validated its struct tags.
func prepareConfig(cfg Config) (Config, error) {
	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// openDatabase constructs the instance's sole primary value. It acquires and
// configures the connection pool but leaves the fallible readiness ping to the
// Init lifecycle stage. Any failure before ownership transfer closes locally
// acquired resources.
func openDatabase(_ plugin.BuildContext, cfg Config) (result *gormlib.DB, err error) {
	dialect, err := dialector(cfg.Driver, cfg.DSN)
	if err != nil {
		return nil, err
	}

	var (
		db          *gormlib.DB
		sqlDB       *sql.DB
		transferred bool
	)
	defer func() {
		if transferred {
			return
		}
		if closeErr := closeDatabase(db, sqlDB); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("xbc gorm: clean up failed construction: %w", closeErr))
		}
	}()

	db, err = gormlib.Open(dialect, &gormlib.Config{
		DisableAutomaticPing:   true,
		PrepareStmt:            cfg.PrepareStmt,
		SkipDefaultTransaction: cfg.SkipDefaultTransaction,
	})
	if err != nil {
		return nil, fmt.Errorf("xbc gorm: open %s database: %w", cfg.Driver, err)
	}

	sqlDB, err = db.DB()
	if err != nil {
		return nil, fmt.Errorf("xbc gorm: obtain %s connection pool: %w", cfg.Driver, err)
	}
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConn)
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConn)
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)

	transferred = true
	return db, nil
}

// initializeDatabase verifies readiness after ownership has transferred to
// XBC. A failed ping is unwound through stopDatabase by the runtime.
func initializeDatabase(db *gormlib.DB, ctx *plugin.Context) error {
	if db == nil {
		return errors.New("xbc gorm: Init requires a non-nil database")
	}
	if ctx == nil {
		return errors.New("xbc gorm: Init requires a non-nil plugin context")
	}

	driver := databaseDriver(db)
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("xbc gorm: obtain %s connection pool: %w", driver, err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("xbc gorm: ping %s database: %w", driver, err)
	}
	return nil
}

// stopDatabase is safe before or after Init and after a failed Init. GORM's
// prepared-statement store and database/sql's Close are concurrency-safe, and
// sql.DB.Close is idempotent, so repeated cleanup remains a no-op.
func stopDatabase(db *gormlib.DB, _ context.Context) error {
	if err := closeDatabase(db, nil); err != nil {
		return fmt.Errorf("xbc gorm: close database: %w", err)
	}
	return nil
}

func validateConfig(cfg Config) error {
	if strings.TrimSpace(cfg.DSN) == "" {
		return errors.New("xbc gorm: dsn is required")
	}
	if _, err := dialector(cfg.Driver, cfg.DSN); err != nil {
		return err
	}
	if cfg.MaxOpenConn < 1 {
		return fmt.Errorf("xbc gorm: max_open_conn must be at least 1, got %d", cfg.MaxOpenConn)
	}
	if cfg.MaxIdleConn < 0 {
		return fmt.Errorf("xbc gorm: max_idle_conn cannot be negative, got %d", cfg.MaxIdleConn)
	}
	if cfg.MaxIdleConn > cfg.MaxOpenConn {
		return fmt.Errorf("xbc gorm: max_idle_conn (%d) cannot exceed max_open_conn (%d)", cfg.MaxIdleConn, cfg.MaxOpenConn)
	}
	if cfg.ConnMaxLifetime < 0 {
		return fmt.Errorf("xbc gorm: conn_max_lifetime cannot be negative, got %s", cfg.ConnMaxLifetime)
	}
	if cfg.ConnMaxIdleTime < 0 {
		return fmt.Errorf("xbc gorm: conn_max_idle_time cannot be negative, got %s", cfg.ConnMaxIdleTime)
	}
	return nil
}

func databaseDriver(db *gormlib.DB) string {
	if db == nil || db.Dialector == nil || db.Dialector.Name() == "" {
		return "database"
	}
	return db.Dialector.Name()
}

func closeDatabase(db *gormlib.DB, sqlDB *sql.DB) error {
	if db == nil && sqlDB == nil {
		return nil
	}
	if db != nil {
		if prepared, ok := db.ConnPool.(*gormlib.PreparedStmtDB); ok {
			prepared.Close()
		}
	}
	if sqlDB == nil && db != nil {
		var err error
		sqlDB, err = db.DB()
		if err != nil {
			return err
		}
	}
	if sqlDB == nil {
		return nil
	}
	return sqlDB.Close()
}
