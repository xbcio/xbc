package oncetoken

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

const (
	// DefaultPrefix is the Redis key prefix used when Config.Prefix is empty.
	DefaultPrefix = "once_token:"

	// DefaultTTL is the lifetime used when Config.DefaultTTL is zero.
	DefaultTTL = 5 * time.Minute

	// tokenEntropyBytes is the number of random bytes behind one token. Sixteen
	// bytes are 128 bits, rendered as 32 hexadecimal characters.
	tokenEntropyBytes = 16
)

var (
	// ErrInvalid reports a token that is empty or not 32 lowercase hexadecimal
	// characters. Consume returns it without touching Redis.
	ErrInvalid = errors.New("oncetoken: invalid token")

	// ErrExpired reports that no token with the given value exists. It covers
	// expiry, an earlier consumption, and a value that was never issued, so a
	// caller cannot use it to distinguish those cases.
	ErrExpired = errors.New("oncetoken: token expired or already consumed")

	// ErrPathMismatch reports that the token was presented from a path other
	// than the one it was issued for. The token has been destroyed by the time
	// this error is returned.
	ErrPathMismatch = errors.New("oncetoken: token is bound to a different path")
)

// tokenPattern is the only token shape Issue produces and Consume accepts.
// The check runs before the Redis key is assembled: the key is Prefix+token,
// so an unconstrained token could address keys outside the store's namespace
// through a colon, a wildcard, or another key's name.
var tokenPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// consumeScript reads the path bound to one token, compares it with the
// caller's path, and deletes the key -- all in a single evaluation. Redis runs
// a script atomically, so concurrent consumers cannot both observe the value:
// exactly one consumes it and every other sees it gone. A path mismatch also
// deletes the key, in that same evaluation, so a wrong-path probe cannot be
// repeated against the same token.
//
// KEYS[1] is the token key and ARGV[1] is the caller's path, where an empty
// path means no comparison. The reply is a two-element array: a status and the
// stored path.
var consumeScript = goredis.NewScript(`
local stored = redis.call('GET', KEYS[1])
if not stored then
  return {'expired', ''}
end
if stored ~= '' and ARGV[1] ~= '' and stored ~= ARGV[1] then
  redis.call('DEL', KEYS[1])
  return {'mismatch', stored}
end
redis.call('DEL', KEYS[1])
return {'ok', stored}
`)

// Config configures a Store. Its zero value is usable: Prefix defaults to
// DefaultPrefix and DefaultTTL to DefaultTTL.
type Config struct {
	// Prefix is prepended to every token to form its Redis key. Deployments
	// that share a Redis instance share tokens when they share a prefix.
	Prefix string

	// DefaultTTL is the lifetime of a token when Issue is not given one. It
	// must not be negative; zero selects DefaultTTL.
	DefaultTTL time.Duration
}

// Store issues and consumes single-use tokens. It is immutable after New, so
// it is safe for concurrent use by multiple goroutines.
type Store struct {
	client     goredis.UniversalClient
	prefix     string
	defaultTTL time.Duration
}

// New builds a Store over client. It returns an error when client is nil
// (including the typed nil that can be stored in the interface) or when
// cfg.DefaultTTL is negative.
func New(client goredis.UniversalClient, cfg Config) (*Store, error) {
	if isNilClient(client) {
		return nil, errors.New("oncetoken: redis client is nil")
	}
	if cfg.DefaultTTL < 0 {
		return nil, errors.New("oncetoken: default TTL must not be negative")
	}
	if cfg.Prefix == "" {
		cfg.Prefix = DefaultPrefix
	}
	if cfg.DefaultTTL == 0 {
		cfg.DefaultTTL = DefaultTTL
	}
	return &Store{client: client, prefix: cfg.Prefix, defaultTTL: cfg.DefaultTTL}, nil
}

// Issue creates a token bound to path and returns it. An empty path leaves the
// token unbound: Consume will accept it from any path. The lifetime is the
// store's default unless a positive ttl is given; a non-positive ttl falls back
// to the default rather than creating a token that never expires.
func (s *Store) Issue(ctx context.Context, path string, ttl ...time.Duration) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", fmt.Errorf("oncetoken: issue: %w", err)
	}
	lifetime := s.defaultTTL
	if len(ttl) > 0 && ttl[0] > 0 {
		lifetime = ttl[0]
	}
	if err := s.client.Set(ctx, s.prefix+token, path, lifetime).Err(); err != nil {
		return "", fmt.Errorf("oncetoken: issue: %w", err)
	}
	return token, nil
}

// Consume validates token, atomically consumes it, and returns the path it was
// issued for. It reports ErrInvalid for a malformed token, ErrExpired when the
// token does not exist, and ErrPathMismatch when path differs from the bound
// path; the mismatch and the destruction of the token happen in one atomic
// script, so the token cannot be probed again. When either the bound path or
// path is empty no comparison is made, which is what keeps an unbound token
// usable from anywhere.
func (s *Store) Consume(ctx context.Context, token, path string) (string, error) {
	if !tokenPattern.MatchString(token) {
		return "", ErrInvalid
	}
	reply, err := consumeScript.Run(ctx, s.client, []string{s.prefix + token}, path).Result()
	if err != nil {
		return "", fmt.Errorf("oncetoken: consume: %w", err)
	}
	parts, ok := stringParts(reply)
	if !ok {
		return "", errors.New("oncetoken: consume returned an unexpected reply")
	}
	switch parts[0] {
	case "ok":
		return parts[1], nil
	case "mismatch":
		return "", ErrPathMismatch
	case "expired":
		return "", ErrExpired
	default:
		return "", errors.New("oncetoken: consume returned an unknown status")
	}
}

// newToken reads 16 bytes from crypto/rand and renders them as lowercase hex. A
// read failure is returned rather than falling back to a weaker source: the
// secrecy of every token depends on this randomness.
func newToken() (string, error) {
	var raw [tokenEntropyBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// stringParts accepts the reply shape Consume expects: exactly two bulk
// strings. go-redis materializes script arrays as []any, so both the string and
// []byte element forms are admitted and anything else is rejected rather than
// indexed blindly.
func stringParts(reply any) ([]string, bool) {
	items, ok := reply.([]any)
	if !ok || len(items) != 2 {
		return nil, false
	}
	parts := make([]string, len(items))
	for index, item := range items {
		switch typed := item.(type) {
		case string:
			parts[index] = typed
		case []byte:
			parts[index] = string(typed)
		default:
			return nil, false
		}
	}
	return parts, true
}

// isNilClient reports whether client carries no client, including the typed nil
// a caller can store in the interface. New accepts the topology-neutral
// interface so that every topology's client is admissible, and a plain nil
// comparison would let a nil *goredis.Client through to fail on the first
// command instead of at construction.
func isNilClient(client goredis.UniversalClient) bool {
	if client == nil {
		return true
	}
	value := reflect.ValueOf(client)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
