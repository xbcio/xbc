package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
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

// Rotation runs as three steps that each address exactly one key, rather than
// one script over both the old and the replacement key. A Redis cluster
// refuses a multi-key script whose keys hash to different slots (CROSSSLOT),
// and session keys carry no hash tag, so the two-key form would make rotation
// the one operation that only works on a standalone deployment. The sources are
// constants so a test can assert that every step stays single-key.
//
// The price is that the sequence is no longer atomic, and the window is worth
// naming: between step 2 and step 3 the old and the replacement ID are both
// valid, and two callers rotating the same ID concurrently with different
// replacements can both succeed, each receiving only the replacement it minted
// itself. Step 1 fails safe -- the old ID is what stops being accepted -- so an
// ID that was rotated away never comes back. Step 2 is idempotent when it is
// handed the exact value it already installed, which is what lets a retry
// finish a rotation whose step 3 did not run.
const (
	// Step 1 checks the old ID and prunes it when its absolute lifetime has
	// passed. 1 means the sequence may proceed, 0 means the old ID is gone.
	redisRotateValidateSource = `
if redis.call('EXISTS', KEYS[1]) == 0 then return 0 end
local absolute = tonumber(redis.call('HGET', KEYS[1], 'absolute'))
local now = tonumber(ARGV[1])
if not absolute or absolute - now < 1 then
  redis.call('DEL', KEYS[1])
  return 0
end
return 1
`
	// Step 2 installs the replacement unless its key is taken: 1 when the
	// replacement is in place (including the exact value this run would have
	// written), -1 when a different session already holds the ID.
	redisRotateInstallSource = `
if redis.call('EXISTS', KEYS[1]) == 1 then
  if redis.call('HGET', KEYS[1], 'value') == ARGV[1] then
    return 1
  end
  return -1
end
local lease = tonumber(ARGV[4])
local remaining = tonumber(ARGV[2]) - tonumber(ARGV[3])
if remaining < lease then lease = remaining end
redis.call('HSET', KEYS[1], 'value', ARGV[1], 'absolute', ARGV[2], 'touched', ARGV[3])
redis.call('PEXPIRE', KEYS[1], lease)
return 1
`
)

var (
	redisRotateValidateScript = goredis.NewScript(redisRotateValidateSource)
	redisRotateInstallScript  = goredis.NewScript(redisRotateInstallSource)
)

// The subject index is one Redis set per subject, holding the IDDigest of each
// session the subject owns. Like rotation, every index command addresses exactly
// one key (a session key or a subject key, never both), so none of it needs a
// single-slot deployment; the sources are constants so a test can assert that.
const (
	// Adds ARGV[1] to the set and lifts the set's TTL to ARGV[2] milliseconds
	// when the set would otherwise expire sooner. The TTL only ever grows: a set
	// outlives every session it indexes, and a short-lived member never shortens
	// the lease a long-lived one needs.
	redisIndexAddSource = `
redis.call('SADD', KEYS[1], ARGV[1])
if redis.call('PTTL', KEYS[1]) < tonumber(ARGV[2]) then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 1
`
	// Deletes the session and returns its stored value (false when absent), so
	// the caller learns which subject's set to clean without a racing read.
	redisDeleteSource = `
local value = redis.call('HGET', KEYS[1], 'value')
redis.call('DEL', KEYS[1])
return value or false
`
)

var (
	redisIndexAddScript = goredis.NewScript(redisIndexAddSource)
	redisDeleteScript   = goredis.NewScript(redisDeleteSource)
)

// RedisStore is a distributed IndexedStore. Lua scripts make idle renewal atomic
// across application replicas. The Redis client remains owned by the base Redis
// plugin and is never closed here.
//
// It addresses whatever topology that client was configured for: every script
// below touches exactly one key, which is what a cluster client requires.
// Rotation is the operation that had to give something up for this: see the
// sequence documented on Rotate.
//
// Sessions live at prefix + IDDigest(id). The subject index lives at
// prefix + "subject:" + IDDigest(subject): a set of session digests, hashed so
// a subject never reaches a key name verbatim, and disjoint from the session
// keys because "subject:" is not hexadecimal. Cross-key steps are separate
// commands, so each carries a window rather than a transaction:
//
//   - Create and Rotate index the session first and store it second. A failure
//     between the two leaves a stale member, which is harmless; it never leaves
//     a live session unindexed.
//   - List and RevokeBySubject prune members whose session is gone. A pruning
//     read that lands in the one round trip between another replica's index
//     write and its session write can remove the member of a session about to
//     appear, leaving that one session unindexed. RevokeBySubject has the same
//     window for a session created while it runs, so callers change the
//     credential first and revoke second, and repeat the revocation when a
//     guarantee against in-flight logins matters.
//   - RevokeBySubject trusts set membership to delete a session; ListBySubject
//     and RevokeByDigest also compare the stored subject with the requested
//     one. Digests are SHA-256 of random IDs, so a member naming another
//     subject's session cannot arise without writing to Redis directly.
//   - LastSeenAt is the time of the last idle renewal recorded by Touch.
type RedisStore struct {
	client goredis.UniversalClient
	prefix string
	now    func() time.Time
}

// NewRedisStore returns a distributed session store. client may be any
// topology's client, the configured Redis plugin's included.
func NewRedisStore(client goredis.UniversalClient, prefix string) (*RedisStore, error) {
	if isNilClient(client) {
		return nil, errors.New("session: Redis client cannot be nil")
	}
	if stringsInvalidPrefix(prefix) {
		return nil, errors.New("session: Redis prefix cannot be empty or contain control characters")
	}
	return &RedisStore{client: client, prefix: prefix, now: time.Now}, nil
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

func stringsInvalidPrefix(prefix string) bool { return prefix == "" || containsControl(prefix) }

func (s *RedisStore) clock() time.Time {
	if s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

func (s *RedisStore) key(id string) string { return s.prefix + IDDigest(id) }

// subjectKey names the set indexing subject's sessions.
func (s *RedisStore) subjectKey(subject string) string {
	return s.prefix + "subject:" + IDDigest(subject)
}

// indexAdd adds digest to subject's set and lifts the set's TTL to ttlMillis.
func (s *RedisStore) indexAdd(ctx context.Context, subject, digest string, ttlMillis int64) error {
	if err := redisIndexAddScript.Run(ctx, s.client, []string{s.subjectKey(subject)}, digest, ttlMillis).Err(); err != nil {
		return fmt.Errorf("session: Redis index: %w", err)
	}
	return nil
}

// discardIndexEntry removes digest from subject's set unless the session key
// still holds a session of that subject. It runs after a failed or refused
// store step, where the error may not say whether the session was written, so
// it decides from the key's state; any doubt leaves the harmless stale member.
func (s *RedisStore) discardIndexEntry(ctx context.Context, subject, digest string) {
	stored, err := s.client.HGet(ctx, s.prefix+digest, "value").Result()
	switch {
	case err == nil:
		if owner, decodeErr := storedSubject(stored); decodeErr == nil && owner == subject {
			return
		}
	case !errors.Is(err, goredis.Nil):
		return
	}
	_ = s.client.SRem(ctx, s.subjectKey(subject), digest).Err()
}

// removeMembers drops members from subject's set, best effort: a member left
// behind is stale and pruned by the next list or revoke.
func (s *RedisStore) removeMembers(ctx context.Context, subject string, members []string) {
	if len(members) == 0 {
		return
	}
	args := make([]any, len(members))
	for i, member := range members {
		args[i] = member
	}
	_ = s.client.SRem(ctx, s.subjectKey(subject), args...).Err()
}

// splitMembers separates a set's members into well-formed digests, which may
// name a session key, and anything else, which is stale by construction and
// never used to build a key.
func splitMembers(members []string) (digests, malformed []string) {
	for _, member := range members {
		if validDigest(member) {
			digests = append(digests, member)
		} else {
			malformed = append(malformed, member)
		}
	}
	return digests, malformed
}

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
	// Index first: a live session is never unindexed. A failure past this point
	// removes the member again unless the key turns out to hold a session of the
	// same subject.
	digest := IDDigest(value.ID)
	if err := s.indexAdd(ctx, value.Subject, digest, value.ExpiresAt.UnixMilli()-now.UnixMilli()); err != nil {
		return err
	}
	created, err := redisCreateScript.Run(ctx, s.client, []string{s.key(value.ID)}, payload, value.ExpiresAt.UnixMilli(), now.UnixMilli(), lease).Int64()
	if err != nil {
		s.discardIndexEntry(ctx, value.Subject, digest)
		return fmt.Errorf("session: Redis create: %w", err)
	}
	if created != 1 {
		s.discardIndexEntry(ctx, value.Subject, digest)
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

// Rotate installs replacement and retires oldID as a sequence of single-key
// steps: validate the old ID, index the replacement, install it, delete the old
// ID, and unindex the old digest. The step sources above state why the sequence
// replaced one atomic script and what the window between the install and the
// delete means. Indexing precedes the install, so a failed index step aborts the
// rotation with the old session untouched; the unindex step runs after the old
// session is gone and, if it fails, leaves only a stale member. The old digest
// is removed from the replacement's subject set, which Manager keeps equal to
// the old session's subject.
//
// Each error names the step that stopped the sequence. A failed first or second
// step leaves the old ID in place, so the caller may retry; a failed third step
// reports an error even though the replacement is installed, and retrying with
// the identical replacement finishes the sequence because the install step
// accepts the value it already wrote.
//
// A replacement already past its absolute lifetime is installed with a
// non-positive lease and does not survive the install step: the sequence still
// reports success, the old ID is still retired, and the caller is left with no
// usable session, as MemoryStore leaves it with a record that Get prunes.
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

	valid, err := redisRotateValidateScript.Run(ctx, s.client, []string{s.key(oldID)}, now.UnixMilli()).Int64()
	if err != nil {
		return fmt.Errorf("session: Redis rotate: %w", err)
	}
	if valid != 1 {
		return ErrNotFound
	}

	// A replacement with no lifetime left is not stored by the install step, so
	// it is not indexed either: its member could never be live, and a set that
	// received it as its only member would be left without a TTL.
	digest := IDDigest(replacement.ID)
	if remaining := replacement.ExpiresAt.UnixMilli() - now.UnixMilli(); remaining >= 1 {
		if err := s.indexAdd(ctx, replacement.Subject, digest, remaining); err != nil {
			return err
		}
	}

	installed, err := redisRotateInstallScript.Run(ctx, s.client, []string{s.key(replacement.ID)}, payload, replacement.ExpiresAt.UnixMilli(), now.UnixMilli(), durationMillis(idleTTL)).Int64()
	if err != nil {
		s.discardIndexEntry(ctx, replacement.Subject, digest)
		return fmt.Errorf("session: Redis rotate: %w", err)
	}
	switch installed {
	case -1:
		s.discardIndexEntry(ctx, replacement.Subject, digest)
		return ErrAlreadyExists
	case 1:
	default:
		return errors.New("session: Redis rotate returned an invalid result")
	}

	if err := s.client.Del(ctx, s.key(oldID)).Err(); err != nil {
		return fmt.Errorf("session: Redis rotate: %w", err)
	}
	s.removeMembers(ctx, replacement.Subject, []string{IDDigest(oldID)})
	return nil
}

func (s *RedisStore) Delete(ctx context.Context, id string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if id == "" {
		return nil
	}
	// The session goes first; the index entry follows and, if its removal fails,
	// stays behind as a stale member.
	stored, err := redisDeleteScript.Run(ctx, s.client, []string{s.key(id)}).Text()
	if errors.Is(err, goredis.Nil) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("session: Redis delete: %w", err)
	}
	if subject, decodeErr := storedSubject(stored); decodeErr == nil && subject != "" {
		s.removeMembers(ctx, subject, []string{IDDigest(id)})
	}
	return nil
}

var _ IndexedStore = (*RedisStore)(nil)

// ListBySubject returns the live sessions of subject. A member whose key is
// gone, whose session expired, or whose stored subject differs is pruned
// instead of reported; a member whose stored value cannot be decoded is
// skipped but kept, so RevokeBySubject can still delete it.
func (s *RedisStore) ListBySubject(ctx context.Context, subject string) ([]SessionInfo, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	subject, err := normalizeSubject(subject)
	if err != nil {
		return nil, err
	}
	members, err := s.client.SMembers(ctx, s.subjectKey(subject)).Result()
	if err != nil {
		return nil, fmt.Errorf("session: Redis list: %w", err)
	}
	digests, stale := splitMembers(members)
	infos := make([]SessionInfo, 0, len(digests))
	if len(digests) > 0 {
		pipe := s.client.Pipeline()
		reads := make([]*goredis.SliceCmd, len(digests))
		for i, digest := range digests {
			reads[i] = pipe.HMGet(ctx, s.prefix+digest, "value", "touched")
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return nil, fmt.Errorf("session: Redis list: %w", err)
		}
		now := s.clock()
		for i, digest := range digests {
			values, err := reads[i].Result()
			if err != nil {
				return nil, fmt.Errorf("session: Redis list: %w", err)
			}
			if len(values) != 2 || values[0] == nil {
				stale = append(stale, digest)
				continue
			}
			value, err := decodeStored(redisString(values[0]), digest)
			if err != nil {
				continue
			}
			if value.Subject != subject || !value.ExpiresAt.After(now) {
				stale = append(stale, digest)
				continue
			}
			var lastSeen time.Time
			if touched, err := strconv.ParseInt(redisString(values[1]), 10, 64); err == nil {
				lastSeen = time.UnixMilli(touched).UTC()
			}
			infos = append(infos, SessionInfo{
				Digest:     digest,
				Subject:    subject,
				Attributes: value.Attributes,
				IssuedAt:   value.IssuedAt,
				ExpiresAt:  value.ExpiresAt,
				LastSeenAt: lastSeen,
			})
		}
	}
	s.removeMembers(ctx, subject, stale)
	return infos, nil
}

// RevokeBySubject deletes every session the subject's set names, then removes
// exactly the members it read. It never deletes the set itself: a member added
// by a concurrent Create must stay indexed. The count is the number of keys
// actually deleted, so stale members are not counted.
func (s *RedisStore) RevokeBySubject(ctx context.Context, subject string) (int, error) {
	if err := contextError(ctx); err != nil {
		return 0, err
	}
	subject, err := normalizeSubject(subject)
	if err != nil {
		return 0, err
	}
	members, err := s.client.SMembers(ctx, s.subjectKey(subject)).Result()
	if err != nil {
		return 0, fmt.Errorf("session: Redis revoke by subject: %w", err)
	}
	digests, _ := splitMembers(members)
	revoked := 0
	if len(digests) > 0 {
		pipe := s.client.Pipeline()
		deletes := make([]*goredis.IntCmd, len(digests))
		for i, digest := range digests {
			deletes[i] = pipe.Del(ctx, s.prefix+digest)
		}
		_, execErr := pipe.Exec(ctx)
		for _, del := range deletes {
			if n, err := del.Result(); err == nil {
				revoked += int(n)
			}
		}
		if execErr != nil {
			// Members stay indexed: some deletions may not have run, and the call
			// is safe to repeat.
			return revoked, fmt.Errorf("session: Redis revoke by subject: %w", execErr)
		}
	}
	s.removeMembers(ctx, subject, members)
	return revoked, nil
}

// RevokeByDigest deletes the session named by digest when it is a member of
// subject's set and its stored subject agrees, and reports whether a live
// session was deleted. Another subject's digest is never touched.
func (s *RedisStore) RevokeByDigest(ctx context.Context, subject, digest string) (bool, error) {
	if err := contextError(ctx); err != nil {
		return false, err
	}
	subject, err := normalizeSubject(subject)
	if err != nil {
		return false, err
	}
	if !validDigest(digest) {
		return false, errors.New("session: digest must be 64 lowercase hex characters")
	}
	member, err := s.client.SIsMember(ctx, s.subjectKey(subject), digest).Result()
	if err != nil {
		return false, fmt.Errorf("session: Redis revoke by digest: %w", err)
	}
	if !member {
		return false, nil
	}
	key := s.prefix + digest
	stored, err := s.client.HGet(ctx, key, "value").Result()
	if errors.Is(err, goredis.Nil) {
		s.removeMembers(ctx, subject, []string{digest})
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("session: Redis revoke by digest: %w", err)
	}
	live := true
	if value, err := decodeStored(stored, digest); err == nil {
		if value.Subject != subject {
			s.removeMembers(ctx, subject, []string{digest})
			return false, nil
		}
		live = value.ExpiresAt.After(s.clock())
	}
	deleted, err := s.client.Del(ctx, key).Result()
	if err != nil {
		return false, fmt.Errorf("session: Redis revoke by digest: %w", err)
	}
	s.removeMembers(ctx, subject, []string{digest})
	return deleted == 1 && live, nil
}

func encodeStored(value Session) ([]byte, error) {
	return json.Marshal(storedSession{
		Subject:    value.Subject,
		Attributes: cloneMap(value.Attributes),
		IssuedAtMS: value.IssuedAt.UnixMilli(),
		ExpiresMS:  value.ExpiresAt.UnixMilli(),
	})
}

// storedSubject reads the subject out of a stored payload without building a
// Session, for callers that have the payload but not the session ID.
func storedSubject(encoded string) (string, error) {
	var stored storedSession
	if err := json.Unmarshal([]byte(encoded), &stored); err != nil {
		return "", err
	}
	return strings.TrimSpace(stored.Subject), nil
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
