package distributed

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// DefaultPrefix is the key namespace used when MapOptions.Prefix is empty.
	DefaultPrefix = "distributed"

	// operationSet carries a new value for one key.
	operationSet = "set"
	// operationDelete removes one key.
	operationDelete = "delete"
	// operationClear removes every key.
	operationClear = "clear"

	// setupTimeout bounds the subscription confirmation and the snapshot read
	// NewMap performs. A caller that passes a shorter deadline still wins; this
	// cap only keeps a store that accepts a connection and stops answering from
	// hanging startup forever.
	setupTimeout = 5 * time.Second
	// closeWaitTimeout bounds how long Close waits for the subscription loop to
	// observe its closed connection. Exceeding it is reported, not ignored.
	closeWaitTimeout = 5 * time.Second
)

// mapNamePattern accepts names and prefixes that are safe in a Redis key and
// readable in configuration. A map name is the last segment of a Redis key;
// a prefix is every segment before ":map", so both use the same charset.
var mapNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func validMapName(name string) bool { return mapNamePattern.MatchString(name) }

// PersistMode selects what a map keeps in the backing store.
type PersistMode string

const (
	// PersistNone broadcasts changes without persisting them. A map created in
	// this mode starts empty, and a subscription that was down while a change
	// was published cannot recover it: there is no snapshot to resynchronize
	// from. Resync reports ErrPersistRequired in this mode.
	PersistNone PersistMode = "none"

	// PersistTTL stores every entry in Redis and applies the map's TTL to the
	// stored hash. The TTL belongs to the collection, not to individual
	// entries: one write refreshes the expiry of every entry the collection
	// holds, because Redis has one expiry per hash.
	PersistTTL PersistMode = "ttl"
)

// MapOptions configures one map. The zero value is valid: it uses the default
// prefix, no persistence, an unlimited local cache, and slog.Default().
type MapOptions[V any] struct {
	// Prefix namespaces every key and channel this map uses. It defaults to
	// DefaultPrefix. Two deployments that share a Redis instance and prefix
	// share map state; give each logical cluster its own prefix. A prefix that
	// is not safe in a Redis key is rejected with ErrInvalidOption.
	Prefix string
	// Decode converts one entry's stored JSON into V on every read path: local
	// writes, peer notifications, and snapshot loads. It receives a private
	// copy of the bytes and may retain them. A decode failure rejects the
	// entry: Set returns ErrDecode and stores nothing; a notification or
	// snapshot entry carrying a value Decode rejects is skipped.
	//
	// A nil Decode unmarshals the bytes into V with encoding/json, which is
	// the right choice when the writer and the reader share one Go type. A
	// writer may call Set with a different type as long as its JSON shape
	// decodes into V.
	Decode func(key string, raw json.RawMessage) (V, error)
	// Persist selects what changes are kept in Redis: PersistNone broadcasts
	// only, PersistTTL stores the map and applies TTL to the stored hash.
	Persist PersistMode
	// TTL is the expiry applied to the whole stored hash in PersistTTL mode.
	// It is required and positive in that mode, and ignored in PersistNone.
	TTL time.Duration
	// MaxSize bounds the local cache with LRU eviction. Zero means unlimited;
	// a negative size is rejected with ErrInvalidOption. Eviction is local
	// only: it never deletes from Redis and never broadcasts.
	MaxSize int
	// Logger receives diagnostics about discarded notifications and snapshot
	// entries. It defaults to slog.Default().
	Logger *slog.Logger
}

// Map is one synchronized string-keyed map.
//
// # Write semantics
//
// Set, Delete, and Clear follow one order: serialize the value, decode it
// locally, persist (in PersistTTL mode), change the local map, then broadcast.
// The consequences are deliberate:
//
//   - A decode failure returns an error wrapping ErrDecode and changes
//     nothing: no local entry, no persisted entry, no message.
//   - A persistence failure returns an error wrapping ErrPersist and changes
//     nothing locally. The local map and Redis stay consistent.
//   - A broadcast failure returns an error wrapping ErrBroadcast after the
//     local map and, in PersistTTL mode, Redis have already changed. The
//     write is not rolled back: rolling back a successful persistence would
//     risk overwriting a newer value written by a peer, and peers that missed
//     the message recover the state from Redis with Resync.
//
// A message carries the identity of the map instance that published it, and
// an instance drops its own messages before any decode or store runs. Without
// that identity every write would be applied twice locally -- once by the
// writer and once by the writer's own subscription -- and a counting decoder
// would observe both.
type Map[V any] struct {
	client redis.UniversalClient
	log    *slog.Logger

	name    string
	hashKey string
	channel string
	origin  string

	decode  func(key string, raw json.RawMessage) (V, error)
	persist PersistMode
	ttl     time.Duration

	store *store[V]

	mu      sync.Mutex
	pubsub  *redis.PubSub
	done    chan struct{}
	serving bool
	closed  bool

	// afterSnapshot is a test seam: it runs at the end of every snapshot read,
	// inside loadSnapshot. That placement is what lets a test inject a change
	// between the snapshot and the listener start, and thus observe whether the
	// subscription already existed at that moment. Production leaves it nil.
	afterSnapshot func()
}

// NewMap opens one synchronized map and returns it ready to read.
//
// The call completes synchronously: it subscribes, confirms the subscription,
// and reads the persisted snapshot before returning, so a caller never
// observes a half-started map. ctx bounds that startup work only; the
// background subscription keeps running until Close.
//
// There is no process-wide registry: two calls with the same name create two
// independent instances that both subscribe to the same channel and observe
// each other's writes. See the package documentation.
func NewMap[V any](ctx context.Context, client redis.UniversalClient, name string, options MapOptions[V]) (*Map[V], error) {
	created, err := newMap(client, name, options)
	if err != nil {
		return nil, err
	}
	if err := created.start(ctx); err != nil {
		return nil, err
	}
	return created, nil
}

// newMap validates the configuration and builds the map without performing
// I/O. NewMap calls it and then starts the map; tests use it directly when
// they need to install a hook between construction and start.
func newMap[V any](client redis.UniversalClient, name string, options MapOptions[V]) (*Map[V], error) {
	if client == nil {
		return nil, errors.New("distributed: NewMap requires a non-nil redis client")
	}
	resolved, err := resolveMapOptions(name, options)
	if err != nil {
		return nil, err
	}
	origin, err := randomOrigin()
	if err != nil {
		return nil, err
	}
	key := resolved.Prefix + ":map:" + name
	return &Map[V]{
		client:  client,
		log:     resolved.Logger,
		name:    name,
		hashKey: key,
		channel: key + ":events",
		origin:  origin,
		decode:  resolved.Decode,
		persist: resolved.Persist,
		ttl:     resolved.TTL,
		store:   newStore[V](resolved.MaxSize),
		done:    make(chan struct{}),
	}, nil
}

// resolveMapOptions applies defaults and enforces every rule that is decidable
// without I/O. It returns a copy so the caller's options are not mutated.
func resolveMapOptions[V any](name string, options MapOptions[V]) (MapOptions[V], error) {
	if !validMapName(name) {
		return MapOptions[V]{}, fmt.Errorf("%w: %q", ErrInvalidName, name)
	}

	options.Prefix = strings.TrimSpace(options.Prefix)
	if options.Prefix == "" {
		options.Prefix = DefaultPrefix
	}
	if !validMapName(options.Prefix) {
		return MapOptions[V]{}, fmt.Errorf("%w: prefix %q must match %s", ErrInvalidOption, options.Prefix, mapNamePattern)
	}

	if options.Persist == "" {
		options.Persist = PersistNone
	}
	switch options.Persist {
	case PersistNone:
	case PersistTTL:
		if options.TTL <= 0 {
			return MapOptions[V]{}, fmt.Errorf("%w: persist ttl requires a positive ttl", ErrInvalidOption)
		}
	default:
		return MapOptions[V]{}, fmt.Errorf("%w: unknown persist mode %q", ErrInvalidOption, options.Persist)
	}
	if options.MaxSize < 0 {
		return MapOptions[V]{}, fmt.Errorf("%w: max size cannot be negative, got %d", ErrInvalidOption, options.MaxSize)
	}

	if options.Decode == nil {
		options.Decode = decodeJSON[V]
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return options, nil
}

// decodeJSON is the default Decode: it unmarshals the stored bytes into V.
func decodeJSON[V any](_ string, raw json.RawMessage) (V, error) {
	var value V
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, err
	}
	return value, nil
}

// randomOrigin returns the identity a map stamps on its own notifications. It
// is drawn per map instance, so two maps of the same name in one process do
// not ignore each other.
func randomOrigin() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("distributed: generate map identity: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

// start establishes the subscription, confirms it, reads the persisted
// snapshot, and only then launches the listener.
//
// The order is the point. A snapshot read before the subscription is confirmed
// leaves a window in which a peer publishes a change that the snapshot does not
// contain and the subscription is not yet listening for; that change would be
// lost. Subscribing first makes the subscription buffer anything published
// during the snapshot read, and the listener drains that buffer when it starts.
func (m *Map[V]) start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	setupCtx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()

	pubsub, events, err := m.subscribeAndLoad(setupCtx)
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.pubsub = pubsub
	m.serving = true
	m.mu.Unlock()

	go m.serve(events)
	return nil
}

// subscribeAndLoad confirms the subscription before reading the snapshot. See
// start for why that order is required.
//
// Confirmation is observed on the subscription's own event channel rather than
// through PubSub.Receive: Receive must not be mixed with a channel consumer, and
// ChannelWithSubscriptions keeps the connection's read loop running during the
// snapshot read, so a message published in that window is buffered for the
// listener instead of being lost.
//
// The ping is not a substitute for the acknowledgement. go-redis negotiates
// RESP3 when the server supports it, and there a subscription acknowledgement is
// a push notification that its receive loop holds until the next command reply
// arrives; without one, confirmation would stall until the health-check ping,
// three seconds later. The ping supplies that reply and also proves the
// connection can round-trip before the map depends on it. Its pong is dropped by
// the event channel.
func (m *Map[V]) subscribeAndLoad(ctx context.Context) (*redis.PubSub, <-chan interface{}, error) {
	pubsub := m.client.Subscribe(ctx, m.channel)
	if err := pubsub.Ping(ctx); err != nil {
		_ = pubsub.Close()
		return nil, nil, fmt.Errorf("distributed: map %q: reach subscription to %q: %w", m.name, m.channel, err)
	}
	events := pubsub.ChannelWithSubscriptions()
	if err := m.awaitSubscription(ctx, events); err != nil {
		_ = pubsub.Close()
		return nil, nil, err
	}
	if m.persist != PersistNone {
		if err := m.loadSnapshot(ctx); err != nil {
			_ = pubsub.Close()
			return nil, nil, err
		}
	}
	return pubsub, events, nil
}

// awaitSubscription waits for the acknowledgement of this map's channel.
// Messages published after the acknowledgement remain buffered in the event
// channel for the listener that starts once the snapshot has been read.
func (m *Map[V]) awaitSubscription(ctx context.Context, events <-chan interface{}) error {
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("distributed: map %q: confirm subscription to %q: %w", m.name, m.channel, ctx.Err())
		case event, ok := <-events:
			if !ok {
				return fmt.Errorf("distributed: map %q: subscription to %q closed before confirmation", m.name, m.channel)
			}
			subscription, ok := event.(*redis.Subscription)
			if !ok {
				continue
			}
			if subscription.Kind == "subscribe" && subscription.Channel == m.channel {
				return nil
			}
		}
	}
}

// serve is the long-lived subscription loop. It ends when the connection
// closes and closes done either way so Close can wait for it.
func (m *Map[V]) serve(events <-chan interface{}) {
	defer close(m.done)
	for {
		event, ok := <-events
		if !ok {
			return
		}
		message, ok := event.(*redis.Message)
		if !ok {
			// Subscription acknowledgements and pongs are not changes.
			continue
		}
		m.apply(message.Payload)
	}
}

// apply folds one notification into the local map. A malformed message, an
// unknown operation, or a decode failure is logged and skipped: storing a
// partially decoded value would leave the local map in a state no reader can
// interpret.
func (m *Map[V]) apply(payload string) {
	var message broadcastMessage
	if err := json.Unmarshal([]byte(payload), &message); err != nil {
		m.warn("distributed: discarding undecodable change notification", "map", m.name)
		return
	}
	if message.Origin == m.origin {
		return
	}
	switch message.Op {
	case operationSet:
		if message.Key == "" {
			m.warn("distributed: discarding set notification without a key", "map", m.name)
			return
		}
		value, err := m.project(message.Key, message.Value)
		if err != nil {
			m.warn("distributed: discarding notification whose value Decode rejected", "map", m.name, "key", message.Key)
			return
		}
		m.store.Set(message.Key, value)
	case operationDelete:
		m.store.Delete(message.Key)
	case operationClear:
		m.store.Clear()
	default:
		m.warn("distributed: discarding unknown change notification", "map", m.name, "op", message.Op)
	}
}

// project runs Decode over a private copy of the serialized value. The copy
// matters because Decode may retain the bytes while the message buffer that
// produced them is reused.
func (m *Map[V]) project(key string, raw []byte) (V, error) {
	copied := make([]byte, len(raw))
	copy(copied, raw)
	return m.decode(key, json.RawMessage(copied))
}

// loadSnapshot replaces the local map with the persisted collection. Entries
// Decode rejects are skipped rather than stored raw.
func (m *Map[V]) loadSnapshot(ctx context.Context) error {
	data, err := m.client.HGetAll(ctx, m.hashKey).Result()
	if err != nil {
		return fmt.Errorf("distributed: map %q: load snapshot from %q: %w", m.name, m.hashKey, err)
	}
	m.replace(data)
	if m.afterSnapshot != nil {
		m.afterSnapshot()
	}
	return nil
}

func (m *Map[V]) replace(data map[string]string) {
	m.store.Clear()
	for key, raw := range data {
		value, err := m.project(key, []byte(raw))
		if err != nil {
			m.warn("distributed: skipping snapshot entry whose value Decode rejected", "map", m.name, "key", key)
			continue
		}
		m.store.Set(key, value)
	}
}

// Set serializes, decodes, persists, stores locally, and broadcasts, in that
// order. See Map for the error contract each step implies.
func (m *Map[V]) Set(ctx context.Context, key string, value any) error {
	if key == "" {
		return ErrInvalidKey
	}
	if err := m.ensureOpen(); err != nil {
		return err
	}
	ctx = orBackground(ctx)
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("distributed: map %q: marshal key %q: %w", m.name, key, err)
	}
	decoded, err := m.project(key, raw)
	if err != nil {
		return fmt.Errorf("%w: map %q key %q: %w", ErrDecode, m.name, key, err)
	}
	if m.persist != PersistNone {
		if err := m.persistSet(ctx, key, raw); err != nil {
			return err
		}
	}
	m.store.Set(key, decoded)
	if err := m.publish(ctx, broadcastMessage{Op: operationSet, Key: key, Value: raw}); err != nil {
		return fmt.Errorf("%w: map %q key %q: local state was already updated: %w", ErrBroadcast, m.name, key, err)
	}
	return nil
}

// persistSet stores one entry and refreshes the collection's TTL in one
// transaction. The hash has one expiry for every entry, so any write extends
// the lifetime of all of them; there is no per-entry expiry to offer here.
func (m *Map[V]) persistSet(ctx context.Context, key string, raw []byte) error {
	_, err := m.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.HSet(ctx, m.hashKey, key, raw)
		if m.ttl > 0 {
			pipe.Expire(ctx, m.hashKey, m.ttl)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%w: map %q key %q: %w", ErrPersist, m.name, key, err)
	}
	return nil
}

// Delete removes one entry. Persistence and the local delete happen before the
// notification, matching Set's ordering and error contract.
func (m *Map[V]) Delete(ctx context.Context, key string) error {
	if key == "" {
		return ErrInvalidKey
	}
	if err := m.ensureOpen(); err != nil {
		return err
	}
	ctx = orBackground(ctx)
	if m.persist != PersistNone {
		if err := m.client.HDel(ctx, m.hashKey, key).Err(); err != nil {
			return fmt.Errorf("%w: map %q key %q: %w", ErrPersist, m.name, key, err)
		}
	}
	m.store.Delete(key)
	if err := m.publish(ctx, broadcastMessage{Op: operationDelete, Key: key}); err != nil {
		return fmt.Errorf("%w: map %q key %q: local state was already updated: %w", ErrBroadcast, m.name, key, err)
	}
	return nil
}

// Clear removes every entry. Persistence and the local clear happen before the
// notification, matching Set's ordering and error contract.
func (m *Map[V]) Clear(ctx context.Context) error {
	if err := m.ensureOpen(); err != nil {
		return err
	}
	ctx = orBackground(ctx)
	if m.persist != PersistNone {
		if err := m.client.Del(ctx, m.hashKey).Err(); err != nil {
			return fmt.Errorf("%w: map %q: %w", ErrPersist, m.name, err)
		}
	}
	m.store.Clear()
	if err := m.publish(ctx, broadcastMessage{Op: operationClear}); err != nil {
		return fmt.Errorf("%w: map %q: local state was already updated: %w", ErrBroadcast, m.name, err)
	}
	return nil
}

// Load returns the locally stored value decoded into V.
func (m *Map[V]) Load(key string) (V, bool) { return m.store.Get(key) }

// Range calls f for each entry in local recency order, most recently used
// first, until f returns false. Range iterates a snapshot of the keys taken
// when it was called; entries added or removed by f are not observed by this
// call.
func (m *Map[V]) Range(f func(key string, value V) bool) { m.store.Range(f) }

// Len reports the number of locally held entries. An entry evicted by MaxSize
// is absent here until a later write, notification, or Resync brings it back.
func (m *Map[V]) Len() int { return m.store.Len() }

// Resync reloads the persisted snapshot and replaces the local map with it. It
// exists for the gap a subscription cannot cover: messages published while a
// subscriber was disconnected are gone, and only the persisted state can
// restore them. In PersistNone mode there is no snapshot and Resync reports
// ErrPersistRequired; missed PersistNone messages are unrecoverable by design.
//
// Resync is a recovery operation, not a consistency mechanism: a write
// concurrent with its snapshot read may be overwritten locally. Callers that
// must not lose concurrent writes serialize the two themselves.
func (m *Map[V]) Resync(ctx context.Context) error {
	if err := m.ensureOpen(); err != nil {
		return err
	}
	if m.persist == PersistNone {
		return ErrPersistRequired
	}
	return m.loadSnapshot(orBackground(ctx))
}

// Close stops this map's subscription and releases it. It is idempotent and
// safe before start. It does not delete persisted data or notify peers; the
// local entries remain readable through Load, Range, and Len, but Set, Delete,
// Clear, and Resync report ErrClosed.
func (m *Map[V]) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	pubsub := m.pubsub
	serving := m.serving
	done := m.done
	m.mu.Unlock()

	var failures []error
	if pubsub != nil {
		if err := pubsub.Close(); err != nil && !errors.Is(err, redis.ErrClosed) {
			failures = append(failures, fmt.Errorf("distributed: map %q: close subscription: %w", m.name, err))
		}
	}
	if serving && done != nil {
		select {
		case <-done:
		case <-time.After(closeWaitTimeout):
			failures = append(failures, fmt.Errorf("distributed: map %q: subscription did not stop within %s", m.name, closeWaitTimeout))
		}
	}
	return errors.Join(failures...)
}

// ensureOpen reports ErrClosed once Close has run.
func (m *Map[V]) ensureOpen() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	return nil
}

// publish broadcasts one notification stamped with this map's identity.
func (m *Map[V]) publish(ctx context.Context, message broadcastMessage) error {
	message.Origin = m.origin
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return m.client.Publish(ctx, m.channel, payload).Err()
}

// orBackground substitutes the background context for a nil one, matching the
// stdlib convention that a nil context means "no deadline".
func orBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// warn logs through the configured logger.
func (m *Map[V]) warn(message string, kv ...any) {
	m.log.Warn(message, kv...)
}

// broadcastMessage is one change notification. Origin is the publishing map
// instance's random identity: a subscriber drops a message whose origin is its
// own, which is what keeps a write from being applied twice locally. Key is
// empty only for a clear operation, which affects the whole map.
type broadcastMessage struct {
	Op     string          `json:"op"`
	Origin string          `json:"origin"`
	Key    string          `json:"key,omitempty"`
	Value  json.RawMessage `json:"value,omitempty"`
}

var (
	// ErrClosed reports that the map has been closed.
	ErrClosed = errors.New("distributed: map is closed")
	// ErrInvalidName reports an empty or malformed map or semaphore name.
	ErrInvalidName = errors.New("distributed: invalid name")
	// ErrInvalidKey reports an empty key.
	ErrInvalidKey = errors.New("distributed: key cannot be empty")
	// ErrInvalidOption reports an option combination this package refuses,
	// such as a negative MaxSize, a malformed Prefix, or PersistTTL without a
	// positive TTL.
	ErrInvalidOption = errors.New("distributed: invalid option")
	// ErrDecode reports that Decode rejected an entry.
	ErrDecode = errors.New("distributed: decode failed")
	// ErrPersist reports that Redis rejected a write. The local map was not
	// changed.
	ErrPersist = errors.New("distributed: persistence failed")
	// ErrBroadcast reports that the change notification could not be
	// published. The local map, and in PersistTTL mode Redis, were already
	// changed.
	ErrBroadcast = errors.New("distributed: broadcast failed")
	// ErrPersistRequired reports Resync on a PersistNone map, which has no
	// snapshot to reload.
	ErrPersistRequired = errors.New("distributed: resync requires persistence")
)
