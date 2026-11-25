package redisx

import "time"

// Option customizes a Config before Open or New constructs a client. Options
// are applied in the order supplied, so a later option wins over an earlier one
// that sets the same field. A nil Option is ignored.
//
// An option only assigns a field; it performs no validation. Open and New
// enforce the mode rules after every option has run, exactly as they do for a
// Config assembled by hand.
type Option func(*Config)

// applyOptions returns a copy of cfg with every non-nil option applied in
// order, so the caller's Config is never mutated. The copy owns its Addrs
// slice, so neither a later mutation of the caller's slice nor an option that
// replaces it can reach the configuration the constructor validates.
func applyOptions(cfg Config, opts []Option) Config {
	applied := cfg
	applied.Addrs = append([]string(nil), cfg.Addrs...)
	for _, option := range opts {
		if option != nil {
			option(&applied)
		}
	}
	return applied
}

// WithMode selects the topology: a standalone server, a sentinel-managed
// master, or a cluster. The mode decides which of the remaining fields apply,
// and an unknown value is rejected at construction.
func WithMode(mode Mode) Option { return func(cfg *Config) { cfg.Mode = mode } }

// WithAddr sets the server address used in standalone mode. Sentinel and
// cluster modes ignore it and name their seeds through WithAddrs.
func WithAddr(addr string) Option { return func(cfg *Config) { cfg.Addr = addr } }

// WithAddrs sets the sentinel or cluster seed addresses, replacing any list
// already present. It is rejected in standalone mode, where WithAddr names the
// single server. The option clones addrs, so later mutation of the caller's
// slice cannot reach the client.
func WithAddrs(addrs ...string) Option {
	return func(cfg *Config) { cfg.Addrs = append([]string(nil), addrs...) }
}

// WithUsername sets the ACL user authenticated against the data nodes.
func WithUsername(username string) Option { return func(cfg *Config) { cfg.Username = username } }

// WithPassword sets the password authenticated against the data nodes for the
// user WithUsername selects.
func WithPassword(password string) Option { return func(cfg *Config) { cfg.Password = password } }

// WithDB selects the logical database. Cluster mode rejects anything but zero
// because the cluster client would ignore the selection.
func WithDB(db int) Option { return func(cfg *Config) { cfg.DB = db } }

// WithMasterName names the master the sentinels report. It is required in
// sentinel mode and rejected elsewhere.
func WithMasterName(masterName string) Option {
	return func(cfg *Config) { cfg.MasterName = masterName }
}

// WithSentinelUsername sets the ACL user authenticated against the sentinel
// nodes, for deployments where that identity differs from the data-node one.
func WithSentinelUsername(username string) Option {
	return func(cfg *Config) { cfg.SentinelUsername = username }
}

// WithSentinelPassword sets the password authenticated against the sentinel
// nodes for the user WithSentinelUsername selects.
func WithSentinelPassword(password string) Option {
	return func(cfg *Config) { cfg.SentinelPassword = password }
}

// WithRouteByLatency routes read-only commands to the closest master or
// replica. It is valid in sentinel mode only and selects the sentinel-backed
// cluster client, the only client that implements the routing.
func WithRouteByLatency(route bool) Option {
	return func(cfg *Config) { cfg.RouteByLatency = route }
}

// WithRouteRandomly routes read-only commands to a random master or replica.
// It is valid in sentinel mode only and selects the sentinel-backed cluster
// client, the only client that implements the routing.
func WithRouteRandomly(route bool) Option {
	return func(cfg *Config) { cfg.RouteRandomly = route }
}

// WithReadOnly enables read-only commands on cluster replica nodes. It is
// valid in cluster mode only.
func WithReadOnly(readOnly bool) Option { return func(cfg *Config) { cfg.ReadOnly = readOnly } }

// WithMaxRedirects bounds MOVED/ASK redirects in cluster mode, where the zero
// value means go-redis's default of 3 and -1 disables redirects. It is valid
// in cluster mode only.
func WithMaxRedirects(maxRedirects int) Option {
	return func(cfg *Config) { cfg.MaxRedirects = maxRedirects }
}

// WithDialTimeout sets the bound on establishing a connection.
func WithDialTimeout(timeout time.Duration) Option {
	return func(cfg *Config) { cfg.DialTimeout = timeout }
}

// WithReadTimeout sets the bound on one read operation, including command
// replies.
func WithReadTimeout(timeout time.Duration) Option {
	return func(cfg *Config) { cfg.ReadTimeout = timeout }
}

// WithWriteTimeout sets the bound on one write operation, including sending a
// command.
func WithWriteTimeout(timeout time.Duration) Option {
	return func(cfg *Config) { cfg.WriteTimeout = timeout }
}

// WithPoolTimeout sets the bound on waiting for a free connection from the
// pool.
func WithPoolTimeout(timeout time.Duration) Option {
	return func(cfg *Config) { cfg.PoolTimeout = timeout }
}

// WithPoolSize sets the number of connections the pool keeps.
func WithPoolSize(size int) Option { return func(cfg *Config) { cfg.PoolSize = size } }

// WithMinIdleConns sets the number of idle connections the pool keeps open.
func WithMinIdleConns(conns int) Option { return func(cfg *Config) { cfg.MinIdleConns = conns } }

// WithMaxIdleConns bounds the idle connections the pool keeps. Zero means no
// explicit limit.
func WithMaxIdleConns(conns int) Option { return func(cfg *Config) { cfg.MaxIdleConns = conns } }

// WithMaxActiveConns bounds the open connections the pool keeps. Zero means
// no explicit limit.
func WithMaxActiveConns(conns int) Option { return func(cfg *Config) { cfg.MaxActiveConns = conns } }

// WithConnMaxIdleTime bounds how long a connection may sit idle before it is
// closed.
func WithConnMaxIdleTime(duration time.Duration) Option {
	return func(cfg *Config) { cfg.ConnMaxIdleTime = duration }
}

// WithConnMaxLifetime bounds how long a connection may be reused. Zero
// disables the limit.
func WithConnMaxLifetime(duration time.Duration) Option {
	return func(cfg *Config) { cfg.ConnMaxLifetime = duration }
}

// WithMaxRetries sets how many times a failed command is retried. -1 disables
// retries; the zero value selects go-redis's default instead of zero retries.
func WithMaxRetries(retries int) Option { return func(cfg *Config) { cfg.MaxRetries = retries } }

// WithPing controls the startup connectivity check. It defaults to true;
// disabling it makes connection establishment lazy, which suits deployments
// that deliberately tolerate an unavailable Redis at startup.
func WithPing(ping bool) Option { return func(cfg *Config) { cfg.Ping = ping } }

// WithTLS replaces the TLS section. The zero TLSConfig keeps the connection in
// cleartext; setting any of its activating fields turns TLS on. The material
// is loaded when the client is constructed, so an unreadable file fails
// construction rather than the first command.
func WithTLS(tlsConfig TLSConfig) Option { return func(cfg *Config) { cfg.TLS = tlsConfig } }
