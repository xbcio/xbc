package redis

import (
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Config configures one standalone Redis client. XBC binds it from one named
// section below plugins.redis (for example, plugins.redis.default).
//
// Zero MaxIdleConns and MaxActiveConns mean no explicit limit. Ping controls
// the startup connectivity check and defaults to true; disabling it makes
// initialization lazy and should be reserved for deployments that deliberately
// tolerate an unavailable Redis at startup.
type Config struct {
	Addr     string `yaml:"addr"     default:"127.0.0.1:6379" validate:"required,hostname_port"`
	Username string `yaml:"username"`
	Password string `yaml:"password" mask:"true"`
	DB       int    `yaml:"db"       default:"0" validate:"min=0"`

	DialTimeout  time.Duration `yaml:"dial_timeout"  default:"5s" validate:"min=1"`
	ReadTimeout  time.Duration `yaml:"read_timeout"  default:"3s" validate:"min=1"`
	WriteTimeout time.Duration `yaml:"write_timeout" default:"3s" validate:"min=1"`
	PoolTimeout  time.Duration `yaml:"pool_timeout"  default:"4s" validate:"min=1"`

	PoolSize       int `yaml:"pool_size"        default:"10" validate:"min=1"`
	MinIdleConns   int `yaml:"min_idle_conns"   default:"0" validate:"min=0,ltefield=PoolSize"`
	MaxIdleConns   int `yaml:"max_idle_conns"   default:"0" validate:"omitempty,min=1,ltefield=PoolSize,gtefield=MinIdleConns"`
	MaxActiveConns int `yaml:"max_active_conns" default:"0" validate:"omitempty,gtefield=PoolSize"`

	ConnMaxIdleTime time.Duration `yaml:"conn_max_idle_time" default:"30m" validate:"min=0"`
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"  default:"0s" validate:"min=0"`
	MaxRetries      int           `yaml:"max_retries"        default:"3" validate:"min=-1"`
	Ping            bool          `yaml:"ping"               default:"true"`
}

func (c Config) options() *goredis.Options {
	return &goredis.Options{
		Addr:            c.Addr,
		Username:        c.Username,
		Password:        c.Password,
		DB:              c.DB,
		DialTimeout:     c.DialTimeout,
		ReadTimeout:     c.ReadTimeout,
		WriteTimeout:    c.WriteTimeout,
		PoolTimeout:     c.PoolTimeout,
		PoolSize:        c.PoolSize,
		MinIdleConns:    c.MinIdleConns,
		MaxIdleConns:    c.MaxIdleConns,
		MaxActiveConns:  c.MaxActiveConns,
		ConnMaxIdleTime: c.ConnMaxIdleTime,
		ConnMaxLifetime: c.ConnMaxLifetime,
		MaxRetries:      c.MaxRetries,
	}
}
