package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// redisConsumeSource deletes the transaction and returns its payload in one
// script, so two concurrent callbacks cannot both observe it. The script
// addresses exactly one key, which keeps it usable on a Redis cluster where
// keys hash to different slots.
const redisConsumeSource = `
local value = redis.call('GET', KEYS[1])
if not value then return false end
redis.call('DEL', KEYS[1])
return value
`

var redisConsumeScript = goredis.NewScript(redisConsumeSource)

// RedisStore shares login transactions and reservations through Redis. It is
// the backend a multi-replica deployment must use: an authorization request
// served by one replica and a callback served by another find the same
// transaction.
//
// The client is owned by the composition that supplied it -- typically the
// Redis plugin instance named by plugins.federation-http.redis_instance --
// so RedisStore opens no connection and implements neither plugin.Drainer nor
// Stop. Every method is safe for concurrent use.
type RedisStore struct {
	client goredis.UniversalClient
	prefix string
	now    func() time.Time
}

var _ Store = (*RedisStore)(nil)

// NewRedisStore returns a store over client whose keys are prefixed with
// prefix. The client must be non-nil; its topology (standalone, sentinel, or
// cluster) is not restricted.
func NewRedisStore(client goredis.UniversalClient, prefix string) (*RedisStore, error) {
	if isNilRedisClient(client) {
		return nil, errors.New("federation: Redis client cannot be nil")
	}
	if strings.TrimSpace(prefix) == "" || containsControl(prefix) {
		return nil, errors.New("federation: Redis prefix cannot be empty or contain control characters")
	}
	return &RedisStore{client: client, prefix: prefix, now: time.Now}, nil
}

func (s *RedisStore) clock() time.Time {
	if s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

// Create stores tx unless a live transaction already holds the same provider
// and state.
func (s *RedisStore) Create(ctx context.Context, tx Transaction) error {
	if s == nil || isNilRedisClient(s.client) {
		return ErrNilStore
	}
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	now := s.clock()
	if err := tx.validate(now); err != nil {
		return err
	}
	ttl := tx.ExpiresAt.Sub(now)
	if ttl < time.Millisecond {
		return fmt.Errorf("%w: lifetime is below the Redis resolution", ErrInvalidTransaction)
	}
	payload, err := json.Marshal(tx)
	if err != nil {
		return fmt.Errorf("%w: encode transaction: %w", ErrInvalidTransaction, err)
	}
	created, err := s.client.SetNX(ctx, transactionKey(s.prefix, tx.Provider, tx.State), payload, ttl).Result()
	if err != nil {
		return fmt.Errorf("%w: create: %w", ErrTransactionStore, err)
	}
	if !created {
		return ErrTransactionExists
	}
	return nil
}

// Consume atomically deletes the transaction and returns it only to the
// browser that started it.
func (s *RedisStore) Consume(ctx context.Context, provider, state, binding string) (Transaction, bool, error) {
	if s == nil || isNilRedisClient(s.client) {
		return Transaction{}, false, ErrNilStore
	}
	if err := checkContext(ctx); err != nil {
		return Transaction{}, false, err
	}
	if !validProviderName(provider) || !validState(state) || !validBindingDigest(binding) {
		return Transaction{}, false, nil
	}

	// Consume must keep issuing this as one script. Replacing it with GET
	// followed by DEL would reintroduce the read-then-delete race the Store
	// contract forbids: two concurrent callbacks would both observe the
	// transaction and both complete the login.
	payload, err := redisConsumeScript.Run(ctx, s.client, []string{transactionKey(s.prefix, provider, state)}).Text()
	switch {
	case errors.Is(err, goredis.Nil):
		return Transaction{}, false, nil
	case err != nil:
		return Transaction{}, false, fmt.Errorf("%w: consume: %w", ErrTransactionStore, err)
	}

	var tx Transaction
	if err := json.Unmarshal([]byte(payload), &tx); err != nil {
		return Transaction{}, false, fmt.Errorf("%w: decode transaction: %w", ErrTransactionStore, err)
	}
	if !tx.ExpiresAt.After(s.clock()) {
		return Transaction{}, false, nil
	}
	if !tx.matches(provider, state, binding) {
		return Transaction{}, false, nil
	}
	return tx, true, nil
}

// SetIfAbsent reserves key under namespace for ttl and reports whether this
// call created it.
func (s *RedisStore) SetIfAbsent(ctx context.Context, namespace, key string, ttl time.Duration) (bool, error) {
	if s == nil || isNilRedisClient(s.client) {
		return false, ErrNilStore
	}
	if err := checkContext(ctx); err != nil {
		return false, err
	}
	if !validReservationNamespace(namespace) || !validReservationKey(key) || !validReservationTTL(ttl) {
		return false, ErrInvalidReservation
	}
	if ttl < time.Millisecond {
		return false, fmt.Errorf("%w: ttl is below the Redis resolution", ErrInvalidReservation)
	}
	created, err := s.client.SetNX(ctx, reservationKey(s.prefix, namespace, key), "1", ttl).Result()
	if err != nil {
		return false, fmt.Errorf("%w: reserve: %w", ErrTransactionStore, err)
	}
	return created, nil
}

// isNilRedisClient reports whether client carries no client, including the
// typed nil a caller can store in the interface.
func isNilRedisClient(client goredis.UniversalClient) bool {
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
