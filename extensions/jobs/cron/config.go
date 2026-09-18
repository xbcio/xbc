package cron

import "time"

// ConcurrencyPolicy controls what a replica does when an earlier invocation of
// the same job is still running on that replica.
type ConcurrencyPolicy string

const (
	// ConcurrencySkip drops the new invocation. The drop is informational and
	// is not reported as a job failure.
	ConcurrencySkip ConcurrencyPolicy = "skip"
	// ConcurrencyDelay waits for the earlier invocation to finish, while still
	// observing shutdown cancellation, and then runs the new invocation.
	ConcurrencyDelay ConcurrencyPolicy = "delay"
)

// Config is bound from plugins.cron.
type Config struct {
	// Seconds selects six-field cron expressions (second through weekday).
	// When false, the standard five-field parser is used. Descriptors such as
	// @hourly and @every are accepted in both modes.
	Seconds        bool              `yaml:"seconds"`
	Timezone       string            `yaml:"timezone"        default:"Local"`
	RunImmediately bool              `yaml:"run_immediately" default:"false"`
	Concurrency    ConcurrencyPolicy `yaml:"concurrency"     default:"skip" validate:"oneof=skip delay"`
	Distributed    DistributedConfig `yaml:"distributed"`
}

// DistributedConfig enables one-execution-across-replicas scheduling.
// Exactly one lock backend must be selected when Enabled is true: either a
// named Redis Definition instance, a Redis.Addr from which cron creates an
// owned client, or one lease.Locker exported by another Definition.
type DistributedConfig struct {
	Enabled   bool   `yaml:"enabled"        default:"false"`
	KeyPrefix string `yaml:"key_prefix"     default:"xbc:cron"`
	// TTL bounds failover after a process or task fault. Jobs may run longer
	// than TTL because the plugin renews their lease while Run is active.
	TTL time.Duration `yaml:"ttl" default:"30s"`
	// RenewInterval defaults to TTL/3 and must not exceed TTL/2, leaving time
	// for at least one retry window before ownership can expire.
	RenewInterval time.Duration `yaml:"renew_interval"`
	RedisInstance string        `yaml:"redis_instance"`
	Redis         RedisConfig   `yaml:"redis"`
}

// RedisConfig configures a client owned by the cron plugin. Leave Addr empty
// when RedisInstance selects a client provided by another plugin.
type RedisConfig struct {
	Addr         string        `yaml:"addr"`
	Username     string        `yaml:"username"`
	Password     string        `yaml:"password" mask:"true"`
	DB           int           `yaml:"db" validate:"min=0"`
	DialTimeout  time.Duration `yaml:"dial_timeout"  default:"5s"`
	ReadTimeout  time.Duration `yaml:"read_timeout"  default:"3s"`
	WriteTimeout time.Duration `yaml:"write_timeout" default:"3s"`
}

func defaultConfig() Config {
	return Config{
		Timezone:    "Local",
		Concurrency: ConcurrencySkip,
		Distributed: DistributedConfig{
			KeyPrefix: "xbc:cron",
			TTL:       30 * time.Second,
			Redis: RedisConfig{
				DialTimeout:  5 * time.Second,
				ReadTimeout:  3 * time.Second,
				WriteTimeout: 3 * time.Second,
			},
		},
	}
}
