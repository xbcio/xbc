package redis

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/extensions/coordination/lease"
	"github.com/xbcio/xbc/plugin"
)

// LeaseKey is the stable identity of the Definition that exports a
// lease.Locker over one configured Redis client.
//
// It is a second Definition rather than a contract on the primary value
// because this plugin's primary is *goredis.Client: a third-party type cannot
// be given a TryAcquire method, so it cannot implement lease.Locker itself.
// Unlike the readiness probe, which aggregates every client this plugin
// produced, a lease belongs to exactly one client, so the section below names
// the instance it is taken over instead of picking one silently.
const LeaseKey plugin.Key = "redis-lease"

// leaseRenewScript and leaseReleaseScript compare the stored owner token before
// acting. Ownership is therefore never transferred by a stale lease: a lease
// whose key has already been taken over renews nothing and deletes nothing.
var (
	leaseRenewScript = goredis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
  return redis.call("pexpire", KEYS[1], ARGV[2])
end
return 0`)
	leaseReleaseScript = goredis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
  return redis.call("del", KEYS[1])
end
return 0`)
)

// LeaseConfig selects the Redis instance whose leases the Definition exports.
//
// It is a section of its own rather than a key of plugins.redis because the
// client section is repeated once per instance and must not carry a Definition
// of its own.
type LeaseConfig struct {
	// Instance names the configured entry below plugins.redis that backs the
	// exported locker. It defaults to the unnamed instance.
	Instance string `yaml:"instance" default:"default"`
}

// leaseDefinition exports one lease.Locker over the selected Redis client. It
// is selected by configuring plugins.redis-lease; the lease capability is
// inert otherwise.
var leaseDefinition = plugin.DefinePlanned(
	LeaseKey,
	plugin.ConfigSpec[LeaseConfig]{
		Defaults: func() LeaseConfig { return LeaseConfig{Instance: plugin.DefaultInstance} },
		Prepare:  prepareLeaseConfig,
	},
	planLease,
	plugin.Options[*locker]{
		Activation: plugin.WhenConfigured("plugins.redis-lease"),
		Exports: plugin.Contracts(
			plugin.ExportAs[lease.Locker](func(value *locker) lease.Locker { return value }),
		),
	},
)

// prepareLeaseConfig validates every configuration-only invariant. It performs
// no I/O and creates no runtime resources, so it is safe to call from planning.
func prepareLeaseConfig(config LeaseConfig) (LeaseConfig, error) {
	if config.Instance == "" {
		config.Instance = plugin.DefaultInstance
	}
	if err := plugin.ValidateInstanceName(config.Instance); err != nil {
		return LeaseConfig{}, fmt.Errorf("redis: invalid plugins.redis-lease.instance: %w", err)
	}
	return config, nil
}

// planLease binds the exact client instance the prepared configuration named.
// Reading a named instance rather than collecting every client is what keeps
// the exported locker unambiguous: one lease belongs to one connection.
//
// It is pure. No client is touched and no connection is opened here: the whole
// plan is a declaration of which already-planned producer the factory reads.
func planLease(config LeaseConfig) (plugin.Plan[*locker], error) {
	client := plugin.RefToInstance[*goredis.Client](Key, config.Instance)
	return plugin.PlanOf(plugin.Inputs(client), func(ctx plugin.BuildContext) (*locker, error) {
		return newLocker(client.Get(ctx).Value)
	}), nil
}

// locker implements lease.Locker with Redis SET NX PX and owner-checking Lua
// scripts. It does not own client; the caller remains responsible for closing
// it. Use NewLocker to reject a nil client early.
type locker struct {
	client *goredis.Client
}

var _ lease.Locker = (*locker)(nil)

// NewLocker returns a lease.Locker backed by client. It is exported for
// composition roots that need a lease before a plugin graph exists: placement
// is decided before a plan is built, so the Locker that decides it cannot come
// from a Definition.
func NewLocker(client *goredis.Client) (lease.Locker, error) {
	implementation, err := newLocker(client)
	if err != nil {
		return nil, err
	}
	return implementation, nil
}

func newLocker(client *goredis.Client) (*locker, error) {
	if client == nil {
		return nil, fmt.Errorf("redis: lease locker requires a non-nil client")
	}
	return &locker{client: client}, nil
}

// TryAcquire atomically creates key with a fresh owner token and a TTL. The
// token is "<claimant>/<random>" when a claimant was named and the random half
// alone when none was, so reading the key answers which process holds it while
// the stored value stays unique to this one acquisition.
func (l *locker) TryAcquire(ctx context.Context, key, claimant string, ttl time.Duration) (lease.Lease, bool, error) {
	if l == nil || l.client == nil {
		return nil, false, fmt.Errorf("redis: lease locker is not initialized")
	}
	if key == "" {
		return nil, false, fmt.Errorf("redis: lease key cannot be empty")
	}
	if ttl < time.Millisecond {
		return nil, false, fmt.Errorf("redis: lease TTL must be at least 1ms, got %s", ttl)
	}
	token, err := leaseOwnerToken(claimant)
	if err != nil {
		return nil, false, err
	}
	acquired, err := l.client.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return nil, false, fmt.Errorf("redis: acquire lease %q: %w", key, err)
	}
	if !acquired {
		return nil, false, nil
	}
	return &redisLease{client: l.client, key: key, owner: token}, true, nil
}

type redisLease struct {
	client *goredis.Client
	key    string
	owner  string
}

var _ lease.Lease = (*redisLease)(nil)

func (l *redisLease) Key() string   { return l.key }
func (l *redisLease) Owner() string { return l.owner }

func (l *redisLease) Renew(ctx context.Context, ttl time.Duration) (bool, error) {
	if ttl < time.Millisecond {
		return false, fmt.Errorf("redis: lease TTL must be at least 1ms, got %s", ttl)
	}
	result, err := leaseRenewScript.Run(
		ctx, l.client, []string{l.key}, l.owner, ttl.Milliseconds(),
	).Int64()
	if err != nil {
		return false, fmt.Errorf("redis: renew lease %q: %w", l.key, err)
	}
	return result == 1, nil
}

func (l *redisLease) Release(ctx context.Context) (bool, error) {
	result, err := leaseReleaseScript.Run(
		ctx, l.client, []string{l.key}, l.owner,
	).Int64()
	if err != nil {
		return false, fmt.Errorf("redis: release lease %q: %w", l.key, err)
	}
	return result == 1, nil
}

// leaseOwnerToken mints one acquisition's owner token: the claimant, when one
// was named, over sixteen bytes of entropy that no other acquisition shares.
//
// The unique half is never optional. A claimant is a configured name that
// survives a restart, so a token that were only the claimant would make a dead
// process's lease compare equal to its successor's -- the stale renewal that
// lease.Lease exists to forbid. The two halves are joined with "/" and the
// unique half contains none, so a reader of the stored value takes everything
// before the last one as the process to go and look at.
func leaseOwnerToken(claimant string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("redis: generate lease owner token: %w", err)
	}
	unique := hex.EncodeToString(raw[:])
	if claimant == "" {
		return unique, nil
	}
	return claimant + "/" + unique, nil
}
