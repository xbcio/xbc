package gormx

import (
	"time"

	gormlogger "gorm.io/gorm/logger"
)

// Option customizes a Config before Open or New constructs a database. Options
// are applied in the order supplied, and a nil Option is ignored.
type Option func(*Config)

// applyOptions returns a copy of cfg with every non-nil option applied in
// order, so the caller's Config is never mutated.
func applyOptions(cfg Config, opts []Option) Config {
	applied := cfg
	for _, option := range opts {
		if option != nil {
			option(&applied)
		}
	}
	return applied
}

// WithDriver selects the SQL dialect. Supported values are mysql (the default
// for New), postgres, sqlite, and sqlserver.
func WithDriver(driver string) Option {
	return func(cfg *Config) { cfg.Driver = driver }
}

// WithDSN sets the driver-specific data source name. It is required; Open and
// New reject a configuration whose DSN is empty or blank.
func WithDSN(dsn string) Option {
	return func(cfg *Config) { cfg.DSN = dsn }
}

// WithMaxOpenConn sets the maximum number of open connections the pool keeps.
// The value must be at least 1.
func WithMaxOpenConn(maxOpenConn int) Option {
	return func(cfg *Config) { cfg.MaxOpenConn = maxOpenConn }
}

// WithMaxIdleConn sets the maximum number of idle connections the pool keeps.
// The value must be between 0 and the maximum open connections.
func WithMaxIdleConn(maxIdleConn int) Option {
	return func(cfg *Config) { cfg.MaxIdleConn = maxIdleConn }
}

// WithConnMaxLifetime sets how long a connection may be reused. Zero disables
// the limit.
func WithConnMaxLifetime(lifetime time.Duration) Option {
	return func(cfg *Config) { cfg.ConnMaxLifetime = lifetime }
}

// WithConnMaxIdleTime sets how long a connection may sit idle before it is
// closed. Zero disables the limit.
func WithConnMaxIdleTime(idleTime time.Duration) Option {
	return func(cfg *Config) { cfg.ConnMaxIdleTime = idleTime }
}

// WithPrepareStmt enables GORM's prepared-statement cache for the session.
func WithPrepareStmt(prepare bool) Option {
	return func(cfg *Config) { cfg.PrepareStmt = prepare }
}

// WithSkipDefaultTransaction disables GORM's wrapping of create, update, and
// delete operations in a transaction.
func WithSkipDefaultTransaction(skip bool) Option {
	return func(cfg *Config) { cfg.SkipDefaultTransaction = skip }
}

// WithTranslateError makes GORM translate driver errors into its portable
// sentinel errors, such as gorm.ErrDuplicatedKey and
// gorm.ErrForeignKeyViolated.
func WithTranslateError(translate bool) Option {
	return func(cfg *Config) { cfg.TranslateError = translate }
}

// WithLogger installs the GORM logger that receives SQL trace and error
// output. A nil logger keeps GORM's default logger.
func WithLogger(logger gormlogger.Interface) Option {
	return func(cfg *Config) { cfg.Logger = logger }
}
