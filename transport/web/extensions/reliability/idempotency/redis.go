package idempotency

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

var acquireScript = goredis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then
  redis.call('HSET', KEYS[1], 'state', 'pending', 'fingerprint', ARGV[1], 'owner', ARGV[2])
  redis.call('PEXPIRE', KEYS[1], ARGV[3])
  return {'acquired'}
end
if redis.call('HGET', KEYS[1], 'fingerprint') ~= ARGV[1] then
  return {'conflict'}
end
local state = redis.call('HGET', KEYS[1], 'state')
if state == 'pending' then
  return {'pending'}
end
if state == 'completed' then
  return {'completed', redis.call('HGET', KEYS[1], 'status') or '', redis.call('HGET', KEYS[1], 'content_type') or '', redis.call('HGET', KEYS[1], 'body') or ''}
end
return {'conflict'}
`)

var completeScript = goredis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return 0 end
if redis.call('HGET', KEYS[1], 'state') ~= 'pending' then return 0 end
if redis.call('HGET', KEYS[1], 'fingerprint') ~= ARGV[1] then return 0 end
if redis.call('HGET', KEYS[1], 'owner') ~= ARGV[2] then return 0 end
redis.call('HSET', KEYS[1], 'state', 'completed', 'owner', '', 'status', ARGV[3], 'content_type', ARGV[4], 'body', ARGV[5])
redis.call('PEXPIRE', KEYS[1], ARGV[6])
return 1
`)

var releaseScript = goredis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return 1 end
if redis.call('HGET', KEYS[1], 'state') ~= 'pending' then return 0 end
if redis.call('HGET', KEYS[1], 'fingerprint') ~= ARGV[1] then return 0 end
if redis.call('HGET', KEYS[1], 'owner') ~= ARGV[2] then return 0 end
redis.call('DEL', KEYS[1])
return 1
`)

// RedisStore is a distributed, Lua-atomic Store over go-redis. It addresses
// whatever topology the client was configured for: every script below touches
// a single key, which is what a cluster client requires.
type RedisStore struct {
	client goredis.UniversalClient
	prefix string
}

// NewRedisStore builds a distributed store. client may be any topology's
// client, the configured Redis plugin's included. key passed to methods should
// already be a fixed-width digest, preventing user-controlled Redis key shape.
func NewRedisStore(client goredis.UniversalClient, prefix string) (*RedisStore, error) {
	if isNilClient(client) {
		return nil, errors.New("idempotency: Redis client cannot be nil")
	}
	if prefix == "" || containsControl(prefix) {
		return nil, errors.New("idempotency: Redis prefix is invalid")
	}
	return &RedisStore{client: client, prefix: prefix}, nil
}

// isNilClient reports whether client carries no client, including the typed nil
// a caller can store in an interface. NewRedisStore accepts the topology-neutral
// interface so that every topology's client is admissible, and a plain nil
// comparison would let a nil *goredis.Client through to fail on the first
// command instead of at construction.
func isNilClient(client goredis.UniversalClient) bool {
	if client == nil {
		return true
	}
	reflected := reflect.ValueOf(client)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func (s *RedisStore) redisKey(key string) string { return s.prefix + key }

// Acquire runs one atomic create/inspect script.
func (s *RedisStore) Acquire(ctx context.Context, key, fingerprint, owner string, pendingTTL time.Duration) (AcquireResult, error) {
	value, err := acquireScript.Run(ctx, s.client, []string{s.redisKey(key)}, fingerprint, owner, ttlMillis(pendingTTL)).Result()
	if err != nil {
		return AcquireResult{}, fmt.Errorf("idempotency: Redis acquire: %w", err)
	}
	parts, err := stringSlice(value)
	if err != nil || len(parts) == 0 {
		return AcquireResult{}, fmt.Errorf("idempotency: Redis acquire returned an invalid result")
	}
	switch parts[0] {
	case "acquired":
		return AcquireResult{State: Acquired}, nil
	case "pending":
		return AcquireResult{State: Pending}, nil
	case "conflict":
		return AcquireResult{State: Conflict}, nil
	case "completed":
		if len(parts) != 4 {
			return AcquireResult{}, fmt.Errorf("idempotency: Redis completed result is incomplete")
		}
		status, parseErr := strconv.Atoi(parts[1])
		if parseErr != nil || status < 200 || status >= 500 {
			return AcquireResult{}, fmt.Errorf("idempotency: Redis stored response status is invalid")
		}
		return AcquireResult{State: Completed, Response: Response{Status: status, ContentType: parts[2], Body: []byte(parts[3])}}, nil
	default:
		return AcquireResult{}, fmt.Errorf("idempotency: Redis acquire returned unknown state %q", parts[0])
	}
}

// Complete atomically verifies ownership and commits a replay response.
func (s *RedisStore) Complete(ctx context.Context, key, fingerprint, owner string, response Response, ttl time.Duration) error {
	result, err := completeScript.Run(ctx, s.client, []string{s.redisKey(key)}, fingerprint, owner, response.Status, response.ContentType, response.Body, ttlMillis(ttl)).Int64()
	if err != nil {
		return fmt.Errorf("idempotency: Redis complete: %w", err)
	}
	if result != 1 {
		return ErrOwnershipLost
	}
	return nil
}

// Release atomically deletes only a matching pending reservation.
func (s *RedisStore) Release(ctx context.Context, key, fingerprint, owner string) error {
	result, err := releaseScript.Run(ctx, s.client, []string{s.redisKey(key)}, fingerprint, owner).Int64()
	if err != nil {
		return fmt.Errorf("idempotency: Redis release: %w", err)
	}
	if result != 1 {
		return ErrOwnershipLost
	}
	return nil
}

func ttlMillis(ttl time.Duration) int64 {
	milliseconds := ttl.Milliseconds()
	if milliseconds < 1 {
		return 1
	}
	return milliseconds
}

func stringSlice(value any) ([]string, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("not an array: %T", value)
	}
	result := make([]string, len(items))
	for i, item := range items {
		switch typed := item.(type) {
		case string:
			result[i] = typed
		case []byte:
			result[i] = string(typed)
		default:
			return nil, fmt.Errorf("array item %d has type %T", i, item)
		}
	}
	return result, nil
}
