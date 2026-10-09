package redis

import (
	"fmt"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Mode selects the Redis topology one configured instance connects to.
type Mode string

const (
	// ModeStandalone connects to the single server named by Addr.
	ModeStandalone Mode = "standalone"
	// ModeSentinel discovers the master named by MasterName through the
	// sentinel addresses in Addrs.
	ModeSentinel Mode = "sentinel"
	// ModeCluster connects to the cluster seeded by Addrs.
	ModeCluster Mode = "cluster"
)

// Config configures one Redis client. XBC binds it from one named section
// below plugins.redis (for example, plugins.redis.default).
//
// Mode decides which topology fields apply; prepareConfig rejects the fields
// that belong to another mode, so one section can never half-describe two
// topologies. Addr names the server in standalone mode and is ignored in the
// other two, where Addrs names the sentinel or cluster seeds instead: its
// default tag means an unset addr is indistinguishable from one spelled out,
// so it cannot be rejected -- only documented as unused.
//
// Zero MaxIdleConns and MaxActiveConns mean no explicit limit. Ping controls
// the startup connectivity check and defaults to true; disabling it makes
// initialization lazy and should be reserved for deployments that deliberately
// tolerate an unavailable Redis at startup.
type Config struct {
	Mode Mode `yaml:"mode" default:"standalone" validate:"required,oneof=standalone sentinel cluster"`

	Addr     string   `yaml:"addr"     default:"127.0.0.1:6379" validate:"required,hostname_port"`
	Addrs    []string `yaml:"addrs"    validate:"omitempty,min=1,dive,required,hostname_port"`
	Username string   `yaml:"username"`
	Password string   `yaml:"password" mask:"true"`
	DB       int      `yaml:"db"       default:"0" validate:"min=0"`

	// MasterName names the master the sentinels report. Required in sentinel
	// mode and rejected elsewhere.
	MasterName string `yaml:"master_name"`
	// SentinelUsername and SentinelPassword authenticate against the sentinel
	// nodes, which are commonly a different ACL identity from the data nodes
	// Username and Password authenticate against.
	SentinelUsername string `yaml:"sentinel_username"`
	SentinelPassword string `yaml:"sentinel_password" mask:"true"`
	// RouteByLatency and RouteRandomly route read-only commands to the closest
	// or a random master or replica. Either one selects the sentinel-backed
	// cluster client, which is the only client that implements the routing:
	// the plain failover client ignores these flags, and go-redis panics when
	// they are handed to it.
	RouteByLatency bool `yaml:"route_by_latency"`
	RouteRandomly  bool `yaml:"route_randomly"`

	// ReadOnly enables read-only commands on cluster replica nodes.
	ReadOnly bool `yaml:"read_only"`
	// MaxRedirects bounds MOVED/ASK redirects in cluster mode. The zero value
	// means the go-redis default of 3; -1 disables redirects entirely. It has
	// no default tag because the tag would fill 3 into standalone and sentinel
	// sections, which must reject the field instead.
	MaxRedirects int `yaml:"max_redirects" validate:"min=-1"`

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

// prepareConfig implements plugin.ConfigSpec.Prepare. It enforces the
// cross-field invariants that per-field validate tags cannot express: which
// fields each topology accepts. Every rule is decidable from the bound
// configuration alone, so prepareConfig performs no I/O and opens no
// connection -- it runs during planning, before construction.
func prepareConfig(config Config) (Config, error) {
	switch config.Mode {
	case ModeStandalone:
		if fields := standaloneRejectedFields(config); len(fields) > 0 {
			return Config{}, fmt.Errorf("redis: mode standalone does not accept %s; use mode sentinel or cluster for them", strings.Join(fields, ", "))
		}
	case ModeSentinel:
		if len(config.Addrs) == 0 {
			return Config{}, fmt.Errorf("redis: mode sentinel requires addrs to name at least one sentinel")
		}
		if config.MasterName == "" {
			return Config{}, fmt.Errorf("redis: mode sentinel requires master_name")
		}
		if fields := clusterOnlyFields(config); len(fields) > 0 {
			return Config{}, fmt.Errorf("redis: mode sentinel does not accept %s; use mode cluster for them", strings.Join(fields, ", "))
		}
	case ModeCluster:
		if len(config.Addrs) == 0 {
			return Config{}, fmt.Errorf("redis: mode cluster requires addrs to name at least one cluster seed")
		}
		if fields := sentinelOnlyFields(config); len(fields) > 0 {
			return Config{}, fmt.Errorf("redis: mode cluster does not accept %s; use mode sentinel for them", strings.Join(fields, ", "))
		}
		if config.DB != 0 {
			return Config{}, fmt.Errorf("redis: mode cluster does not support db, got %d; the cluster client would ignore it", config.DB)
		}
	default:
		return Config{}, fmt.Errorf("redis: unknown mode %q, want standalone, sentinel, or cluster", config.Mode)
	}
	return config, nil
}

// standaloneRejectedFields lists, in configuration spelling, every field a
// standalone section leaves unset. It is the union of the fields the other two
// modes own, so a section that describes a sentinel or cluster deployment
// without saying so is rejected rather than half-applied.
func standaloneRejectedFields(config Config) []string {
	fields := make([]string, 0, 8)
	if len(config.Addrs) > 0 {
		fields = append(fields, "addrs")
	}
	fields = append(fields, sentinelOnlyFields(config)...)
	return append(fields, clusterOnlyFields(config)...)
}

func sentinelOnlyFields(config Config) []string {
	var fields []string
	if config.MasterName != "" {
		fields = append(fields, "master_name")
	}
	if config.SentinelUsername != "" {
		fields = append(fields, "sentinel_username")
	}
	if config.SentinelPassword != "" {
		fields = append(fields, "sentinel_password")
	}
	if config.RouteByLatency {
		fields = append(fields, "route_by_latency")
	}
	if config.RouteRandomly {
		fields = append(fields, "route_randomly")
	}
	return fields
}

func clusterOnlyFields(config Config) []string {
	var fields []string
	if config.ReadOnly {
		fields = append(fields, "read_only")
	}
	if config.MaxRedirects != 0 {
		fields = append(fields, "max_redirects")
	}
	return fields
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

// failoverOptions maps the sentinel fields. It is called only in sentinel mode,
// where prepareConfig has already established that addrs and master_name are
// set and the cluster-only fields are not.
func (c Config) failoverOptions() *goredis.FailoverOptions {
	return &goredis.FailoverOptions{
		MasterName:       c.MasterName,
		SentinelAddrs:    c.Addrs,
		SentinelUsername: c.SentinelUsername,
		SentinelPassword: c.SentinelPassword,
		RouteByLatency:   c.RouteByLatency,
		RouteRandomly:    c.RouteRandomly,
		DB:               c.DB,
		Username:         c.Username,
		Password:         c.Password,
		DialTimeout:      c.DialTimeout,
		ReadTimeout:      c.ReadTimeout,
		WriteTimeout:     c.WriteTimeout,
		PoolTimeout:      c.PoolTimeout,
		PoolSize:         c.PoolSize,
		MinIdleConns:     c.MinIdleConns,
		MaxIdleConns:     c.MaxIdleConns,
		MaxActiveConns:   c.MaxActiveConns,
		ConnMaxIdleTime:  c.ConnMaxIdleTime,
		ConnMaxLifetime:  c.ConnMaxLifetime,
		MaxRetries:       c.MaxRetries,
	}
}

// clusterOptions maps the cluster fields. MaxRetries is mapped and not left to
// the cluster client's own default of -1 (retries off, because the client
// retries redirects internally): the framework's max_retries means the same
// thing in every mode, and a configured value that one mode silently dropped
// would be worse than a documented policy. A cluster deployment that wants the
// library behaviour sets max_retries: -1.
func (c Config) clusterOptions() *goredis.ClusterOptions {
	return &goredis.ClusterOptions{
		Addrs:           c.Addrs,
		Username:        c.Username,
		Password:        c.Password,
		ReadOnly:        c.ReadOnly,
		MaxRedirects:    c.MaxRedirects,
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

// topology names the configured deployment for error messages. It names
// credentials never, and in the addressing modes it names the seeds rather than
// the ignored Addr.
func (c Config) topology() string {
	switch c.Mode {
	case ModeSentinel:
		return fmt.Sprintf("sentinel master %q at %s", c.MasterName, strings.Join(c.Addrs, ", "))
	case ModeCluster:
		return fmt.Sprintf("cluster %s", strings.Join(c.Addrs, ", "))
	default:
		return c.Addr
	}
}
