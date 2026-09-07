package cron

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	redis "github.com/redis/go-redis/v9"
)

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

// RedisLocker implements Locker with Redis SET NX PX and owner-checking Lua
// scripts. It does not own client; the caller remains responsible for closing
// it. Use NewRedisLocker to reject a nil client early.
type RedisLocker struct {
	client *redis.Client
}

var _ Locker = (*RedisLocker)(nil)

// NewRedisLocker constructs an owner-safe Redis-backed Locker.
func NewRedisLocker(client *redis.Client) (*RedisLocker, error) {
	if client == nil {
		return nil, fmt.Errorf("cron: Redis locker requires a non-nil client")
	}
	return &RedisLocker{client: client}, nil
}

// TryAcquire atomically creates key with a random owner token and a TTL.
func (l *RedisLocker) TryAcquire(ctx context.Context, key string, ttl time.Duration) (Lease, bool, error) {
	if l == nil || l.client == nil {
		return nil, false, fmt.Errorf("cron: Redis locker is not initialized")
	}
	if key == "" {
		return nil, false, fmt.Errorf("cron: lock key cannot be empty")
	}
	if ttl < time.Millisecond {
		return nil, false, fmt.Errorf("cron: Redis lock TTL must be at least 1ms, got %s", ttl)
	}
	token, err := ownerToken()
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

var _ Lease = (*redisLease)(nil)

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

func ownerToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("cron: generate lease owner token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
