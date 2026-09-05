package gorm

import "time"

const (
	defaultDriver          = "mysql"
	defaultMaxOpenConn     = 20
	defaultMaxIdleConn     = 10
	defaultConnMaxLifetime = time.Hour
	defaultConnMaxIdleTime = 30 * time.Minute
)

// Config describes one named GORM connection. Each map entry below
// plugins.gorm is bound to a fresh Config because the canonical Definition
// declares multiple instances.
//
// Pool defaults deliberately bound resource use while recycling connections
// before they tend to become stale. A duration of zero may be configured for
// either lifetime field to disable that limit. GORM's default transaction is
// kept unless SkipDefaultTransaction is explicitly enabled.
type Config struct {
	Driver string `yaml:"driver" default:"mysql" validate:"required,oneof=mysql postgres sqlite sqlserver"`
	DSN    string `yaml:"dsn" validate:"required"`

	MaxOpenConn     int           `yaml:"max_open_conn" default:"20" validate:"min=1"`
	MaxIdleConn     int           `yaml:"max_idle_conn" default:"10" validate:"min=0,ltefield=MaxOpenConn"`
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime" default:"1h" validate:"gte=0"`
	ConnMaxIdleTime time.Duration `yaml:"conn_max_idle_time" default:"30m" validate:"gte=0"`

	PrepareStmt            bool `yaml:"prepare_stmt" default:"false"`
	SkipDefaultTransaction bool `yaml:"skip_default_transaction" default:"false"`
}

func defaultConfig() Config {
	return Config{
		Driver:                 defaultDriver,
		MaxOpenConn:            defaultMaxOpenConn,
		MaxIdleConn:            defaultMaxIdleConn,
		ConnMaxLifetime:        defaultConnMaxLifetime,
		ConnMaxIdleTime:        defaultConnMaxIdleTime,
		PrepareStmt:            false,
		SkipDefaultTransaction: false,
	}
}
