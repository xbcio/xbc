package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

type storedSession struct {
	Subject    string         `json:"subject"`
	Attributes map[string]any `json:"attributes,omitempty"`
	IssuedAtMS int64          `json:"issued_at_ms"`
	ExpiresMS  int64          `json:"expires_at_ms"`
}

var redisCreateScript = goredis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then return 0 end
redis.call('HSET', KEYS[1], 'value', ARGV[1], 'absolute', ARGV[2], 'touched', ARGV[3])
redis.call('PEXPIRE', KEYS[1], ARGV[4])
return 1
`)

var redisTouchScript = goredis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return {} end
local absolute = tonumber(redis.call('HGET', KEYS[1], 'absolute'))
local now = tonumber(ARGV[1])
if not absolute or absolute <= now then
  redis.call('DEL', KEYS[1])
  return {}
end
local value = redis.call('HGET', KEYS[1], 'value')
if not value then
  redis.call('DEL', KEYS[1])
  return {}
end
local touched = tonumber(redis.call('HGET', KEYS[1], 'touched')) or 0
local minimum = tonumber(ARGV[3])
if minimum == 0 or now - touched >= minimum then
  local lease = tonumber(ARGV[2])
  local remaining = absolute - now
  if remaining < lease then lease = remaining end
  if lease < 1 then
    redis.call('DEL', KEYS[1])
    return {}
  end
  redis.call('HSET', KEYS[1], 'touched', ARGV[1])
  redis.call('PEXPIRE', KEYS[1], lease)
end
return {value}
`)

var redisRotateScript = goredis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return 0 end
if redis.call('EXISTS', KEYS[2]) == 1 then return -1 end
local absolute = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
if not absolute or absolute <= now then
  redis.call('DEL', KEYS[1])
  return 0
end
local lease = tonumber(ARGV[4])
local remaining = absolute - now
if remaining < lease then lease = remaining end
if lease < 1 then
  redis.call('DEL', KEYS[1])
  return 0
end
redis.call('HSET', KEYS[2], 'value', ARGV[1], 'absolute', ARGV[2], 'touched', ARGV[3])
redis.call('PEXPIRE', KEYS[2], lease)
redis.call('DEL', KEYS[1])
return 1
`)

// RedisStore is a distributed Store. Lua scripts make idle renewal and ID
// rotation atomic across application replicas. The Redis client remains owned
// by the base Redis plugin and is never closed here.
type RedisStore struct {
	client *goredis.Client
	prefix string
	now    func() time.Time
}

// NewRedisStore returns a distributed session store.
func NewRedisStore(client *goredis.Client, prefix string) (*RedisStore, error) {
	if client == nil {
		return nil, errors.New("session: Redis client cannot be nil")
	}
	if stringsInvalidPrefix(prefix) {
		return nil, errors.New("session: Redis prefix cannot be empty or contain control characters")
	}
	return &RedisStore{client: client, prefix: prefix, now: time.Now}, nil
}

func stringsInvalidPrefix(prefix string) bool { return prefix == "" || containsControl(prefix) }

func (s *RedisStore) clock() time.Time {
	if s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

func (s *RedisStore) key(id string) string { return s.prefix + IDDigest(id) }

func (s *RedisStore) Create(ctx context.Context, value Session, idleTTL time.Duration) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	value, err := normalizeSession(value)
	if err != nil {
		return err
	}
	if idleTTL <= 0 {
		return errors.New("session: idle TTL must be positive")
	}
	now := s.clock()
	lease := leaseMillis(now, value.ExpiresAt, idleTTL)
	if lease < 1 {
		return ErrNotFound
	}
	payload, err := encodeStored(value)
	if err != nil {
		return err
	}
	created, err := redisCreateScript.Run(ctx, s.client, []string{s.key(value.ID)}, payload, value.ExpiresAt.UnixMilli(), now.UnixMilli(), lease).Int64()
	if err != nil {
		return fmt.Errorf("session: Redis create: %w", err)
	}
	if created != 1 {
		return ErrAlreadyExists
	}
	return nil
}

func (s *RedisStore) Get(ctx context.Context, id string) (Session, bool, error) {
	if err := contextError(ctx); err != nil {
		return Session{}, false, err
	}
	if id == "" {
		return Session{}, false, nil
	}
	values, err := s.client.HMGet(ctx, s.key(id), "value", "absolute").Result()
	if err != nil {
		return Session{}, false, fmt.Errorf("session: Redis get: %w", err)
	}
	if len(values) != 2 || values[0] == nil || values[1] == nil {
		return Session{}, false, nil
	}
	value, err := decodeStored(redisString(values[0]), id)
	if err != nil {
		return Session{}, false, fmt.Errorf("session: Redis stored value is invalid: %w", err)
	}
	if !value.ExpiresAt.After(s.clock()) {
		_ = s.client.Del(ctx, s.key(id)).Err()
		return Session{}, false, nil
	}
	return value, true, nil
}

func (s *RedisStore) Touch(ctx context.Context, id string, idleTTL, minInterval time.Duration) (Session, bool, error) {
	if err := contextError(ctx); err != nil {
		return Session{}, false, err
	}
	if id == "" || idleTTL <= 0 || minInterval < 0 {
		return Session{}, false, errors.New("session: invalid touch arguments")
	}
	result, err := redisTouchScript.Run(ctx, s.client, []string{s.key(id)}, s.clock().UnixMilli(), durationMillis(idleTTL), durationMillis(minInterval)).Result()
	if err != nil {
		return Session{}, false, fmt.Errorf("session: Redis touch: %w", err)
	}
	parts, ok := result.([]any)
	if !ok || len(parts) == 0 {
		return Session{}, false, nil
	}
	if len(parts) != 1 {
		return Session{}, false, errors.New("session: Redis touch returned an invalid result")
	}
	value, err := decodeStored(redisString(parts[0]), id)
	if err != nil {
		return Session{}, false, fmt.Errorf("session: Redis stored value is invalid: %w", err)
	}
	return value, true, nil
}

func (s *RedisStore) Rotate(ctx context.Context, oldID string, replacement Session, idleTTL time.Duration) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	replacement, err := normalizeSession(replacement)
	if err != nil {
		return err
	}
	if oldID == "" || oldID == replacement.ID || idleTTL <= 0 {
		return errors.New("session: invalid rotation arguments")
	}
	now := s.clock()
	payload, err := encodeStored(replacement)
	if err != nil {
		return err
	}
	result, err := redisRotateScript.Run(ctx, s.client, []string{s.key(oldID), s.key(replacement.ID)}, payload, replacement.ExpiresAt.UnixMilli(), now.UnixMilli(), durationMillis(idleTTL)).Int64()
	if err != nil {
		return fmt.Errorf("session: Redis rotate: %w", err)
	}
	switch result {
	case 1:
		return nil
	case 0:
		return ErrNotFound
	case -1:
		return ErrAlreadyExists
	default:
		return errors.New("session: Redis rotate returned an invalid result")
	}
}

func (s *RedisStore) Delete(ctx context.Context, id string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if id == "" {
		return nil
	}
	if err := s.client.Del(ctx, s.key(id)).Err(); err != nil {
		return fmt.Errorf("session: Redis delete: %w", err)
	}
	return nil
}

func encodeStored(value Session) ([]byte, error) {
	return json.Marshal(storedSession{
		Subject:    value.Subject,
		Attributes: cloneMap(value.Attributes),
		IssuedAtMS: value.IssuedAt.UnixMilli(),
		ExpiresMS:  value.ExpiresAt.UnixMilli(),
	})
}

func decodeStored(encoded string, id string) (Session, error) {
	var stored storedSession
	if err := json.Unmarshal([]byte(encoded), &stored); err != nil {
		return Session{}, err
	}
	return normalizeSession(Session{
		ID:         id,
		Subject:    stored.Subject,
		Attributes: stored.Attributes,
		IssuedAt:   time.UnixMilli(stored.IssuedAtMS),
		ExpiresAt:  time.UnixMilli(stored.ExpiresMS),
	})
}

func redisString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	default:
		return ""
	}
}

func durationMillis(value time.Duration) int64 {
	milliseconds := value.Milliseconds()
	if milliseconds < 1 {
		return 1
	}
	return milliseconds
}

func leaseMillis(now, absolute time.Time, idleTTL time.Duration) int64 {
	remaining := absolute.Sub(now)
	if idleTTL < remaining {
		remaining = idleTTL
	}
	return remaining.Milliseconds()
}
