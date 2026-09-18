package cron

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	redis "github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/extensions/coordination/lease"
)

// This file is cron's own Redis implementation of the lease contract, and it is
// a deliberate second copy: the Redis extension beneath extensions/storage
// carries an equivalent one behind its redis-lease Definition, down to the same
// two Lua scripts.
//
// Sharing a single copy would mean importing that plugin module. Go's unit of
// compilation is the package, and the exported constructor over there sits in
// the same package as that plugin's Definitions and its readiness probe, so one
// call would pull the whole Redis extension -- every Definition it declares,
// its configuration sections, its health contributor -- into cron's production
// closure. That would be the repository's only production edge from one
// capability plugin module to another, and it would resolve through the
// workspace alone: extensions/jobs/cron does not require its sibling, and an
// unpublished sibling requirement is exactly what this repository's module
// rules exist to prevent. A plugin that a downstream application can select on
// its own cannot depend on a plugin that application did not select.
//
// The duplication costs nothing in dependencies. cron already requires go-redis
// directly, because distributed.redis.addr lets it own a client, so this file
// adds no third-party dependency and no import beyond the lease contract cron
// already consumes.
//
// The two implementations are independent implementations of one contract
// rather than a fork of a shared one, and they serve different callers:
//
//   - the redis-lease Definition exports lease.Locker into the plugin graph over
//     a client that graph configured, and its package separately exports
//     NewLocker for composition roots that need a lease before a graph exists,
//     because workload placement is decided before the assembly plan is built.
//   - the locker below is cron's internal convenience path, reached only from
//     newConfiguredPlugin once distributed.redis_instance or
//     distributed.redis.addr has named the backend. It stays unexported: the
//     vocabulary belongs to extensions/coordination/lease, and cron publishes
//     no locking vocabulary of its own. An application that wants one locker
//     shared by several plugins configures plugins.redis-lease and lets cron
//     take lease.Locker as an input, the path that keeps priority over both
//     convenience keys.
//
// What has to stay in step between the two is the contract's ownership rule
// rather than the code: the scripts below compare the stored owner token before
// acting, so a lease whose key has already been taken over renews nothing and
// deletes nothing. TestRedisLockerContentionAndOwnerSafeRelease drives that
// property here, as its counterpart does in the Redis extension.
var (
	redisRenewScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
  return redis.call("pexpire", KEYS[1], ARGV[2])
end
return 0`)
	redisReleaseScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
  return redis.call("del", KEYS[1])
end
return 0`)
)

// redisLocker implements lease.Locker with Redis SET NX PX and owner-checking
// Lua scripts. It does not own client; whoever created the client remains
// responsible for closing it -- for the addr path that is the Plugin itself,
// which closes the client it opened during finalization. Use newRedisLocker to
// reject a nil client early.
type redisLocker struct {
	client *redis.Client
}

var _ lease.Locker = (*redisLocker)(nil)

// newRedisLocker constructs an owner-safe Redis-backed lease.Locker.
func newRedisLocker(client *redis.Client) (*redisLocker, error) {
	if client == nil {
		return nil, fmt.Errorf("cron: Redis locker requires a non-nil client")
	}
	return &redisLocker{client: client}, nil
}

// TryAcquire atomically creates key with a fresh owner token and a TTL. The
// token carries claimant when one was named, exactly as the Redis extension's
// copy does; cron's own scheduler names none, and see the call site for why.
func (l *redisLocker) TryAcquire(ctx context.Context, key, claimant string, ttl time.Duration) (lease.Lease, bool, error) {
	if l == nil || l.client == nil {
		return nil, false, fmt.Errorf("cron: Redis locker is not initialized")
	}
	if key == "" {
		return nil, false, fmt.Errorf("cron: lock key cannot be empty")
	}
	if ttl < time.Millisecond {
		return nil, false, fmt.Errorf("cron: Redis lock TTL must be at least 1ms, got %s", ttl)
	}
	token, err := ownerToken(claimant)
	if err != nil {
		return nil, false, err
	}
	acquired, err := l.client.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return nil, false, fmt.Errorf("cron: acquire Redis lease %q: %w", key, err)
	}
	if !acquired {
		return nil, false, nil
	}
	return &redisLease{client: l.client, key: key, owner: token}, true, nil
}

type redisLease struct {
	client *redis.Client
	key    string
	owner  string
}

var _ lease.Lease = (*redisLease)(nil)

func (l *redisLease) Key() string   { return l.key }
func (l *redisLease) Owner() string { return l.owner }

func (l *redisLease) Renew(ctx context.Context, ttl time.Duration) (bool, error) {
	if ttl < time.Millisecond {
		return false, fmt.Errorf("cron: Redis lease TTL must be at least 1ms, got %s", ttl)
	}
	result, err := redisRenewScript.Run(
		ctx, l.client, []string{l.key}, l.owner, ttl.Milliseconds(),
	).Int64()
	if err != nil {
		return false, fmt.Errorf("cron: renew Redis lease %q: %w", l.key, err)
	}
	return result == 1, nil
}

func (l *redisLease) Release(ctx context.Context) (bool, error) {
	result, err := redisReleaseScript.Run(
		ctx, l.client, []string{l.key}, l.owner,
	).Int64()
	if err != nil {
		return false, fmt.Errorf("cron: release Redis lease %q: %w", l.key, err)
	}
	return result == 1, nil
}

// ownerToken mints one acquisition's owner token: the claimant, when one was
// named, over sixteen bytes of entropy no other acquisition shares. The unique
// half is never optional -- see lease.Locker for why a token that were only the
// claimant would let a stale lease renew its successor's lock.
func ownerToken(claimant string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("cron: generate lease owner token: %w", err)
	}
	unique := hex.EncodeToString(raw[:])
	if claimant == "" {
		return unique, nil
	}
	return claimant + "/" + unique, nil
}
