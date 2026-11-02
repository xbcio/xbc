package redissyncmap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/extensions/coordination/syncmap"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

const (
	// operationSet carries a new value for one key.
	operationSet = "set"
	// operationDelete removes one key.
	operationDelete = "delete"
	// operationClear removes every key.
	operationClear = "clear"

	// setupTimeout bounds the subscription confirmation and the snapshot read
	// that a starting map performs. Start runs before the process serves
	// traffic, so a store that accepts a connection and stops answering must
	// fail the startup rather than hang it.
	setupTimeout = 5 * time.Second
	// destroyWaitTimeout bounds how long Destroy waits for a subscription loop
	// to observe its closed connection. Exceeding it is reported, not ignored.
	destroyWaitTimeout = 5 * time.Second
)

// mapNamePattern accepts names that are safe in a Redis key and readable in a
// configuration file. It is deliberately independent of plugin instance names:
// map names appear inside one namespace's key prefix, not in configuration
// paths.
var mapNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func validMapName(name string) bool { return mapNamePattern.MatchString(name) }

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

// syncMap is one concrete map. It holds transformed values locally, persists
// raw JSON when the mode requires it, and applies peer notifications through
// the same transform path a local write uses, so local and remote reads see
// the same value shape.
type syncMap struct {
	client redis.UniversalClient
	log    log.Logger

	name    string
	hashKey string
	channel string
	origin  string

	transform syncmap.TransformFunc
	persist   syncmap.PersistMode
	ttl       time.Duration

	store *store

	mu        sync.Mutex
	pubsub    *redis.PubSub
	events    <-chan interface{}
	done      chan struct{}
	serving   bool
	destroyed bool

	// afterSnapshot is a test seam: it runs at the end of every snapshot read,
	// inside loadSnapshot. That placement is what lets a test inject a change
	// between the snapshot and whatever the implementation does next, and thus
	// observe whether the subscription already existed at that moment.
	// Production leaves it nil.
	afterSnapshot func()
}

var _ syncmap.Map = (*syncMap)(nil)

func newSyncMap(client redis.UniversalClient, logger log.Logger, namespace, name string, options syncmap.Options) (*syncMap, error) {
	origin, err := randomOrigin()
	if err != nil {
		return nil, err
	}
	key := "xbc-syncmap:" + namespace + ":" + name
	return &syncMap{
		client:    client,
		log:       logger,
		name:      name,
		hashKey:   key,
		channel:   key + ":events",
		origin:    origin,
		transform: options.Transform,
		persist:   options.Persist,
		ttl:       options.TTL,
		store:     newStore(options.MaxSize),
		done:      make(chan struct{}),
	}, nil
}

// randomOrigin returns the identity a map stamps on its own notifications. It
// is drawn per map instance, so two maps of the same name in one process (for
// example across two tests sharing a Redis) do not ignore each other.
func randomOrigin() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("redis-syncmap: generate map identity: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

// start establishes the subscription, confirms it, reads the persisted
// snapshot, and only then launches the listener as a managed task.
//
// The order is the point. A snapshot read before the subscription is confirmed
// leaves a window in which a peer publishes a change that the snapshot does not
// contain and the subscription is not yet listening for; that change would be
// lost. Subscribing first makes the subscription buffer anything published
// during the snapshot read, and the listener drains that buffer when it starts.
func (m *syncMap) start(ctx *plugin.Context) error {
	m.mu.Lock()
	if m.destroyed {
		m.mu.Unlock()
		return syncmap.ErrClosed
	}
	if m.serving {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	pubsub, events, err := m.subscribeAndLoad()
	if err != nil {
		return err
	}

	m.mu.Lock()
	if m.destroyed {
		m.mu.Unlock()
		_ = pubsub.Close()
		return syncmap.ErrClosed
	}
	m.pubsub = pubsub
	m.events = events
	m.mu.Unlock()

	if !ctx.Go(func(taskCtx context.Context) { m.serve(taskCtx) }) {
		_ = m.closeSubscription()
		return fmt.Errorf("redis-syncmap: map %q: the runtime refused the subscription task", m.name)
	}

	m.mu.Lock()
	m.serving = true
	m.mu.Unlock()
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
func (m *syncMap) subscribeAndLoad() (*redis.PubSub, <-chan interface{}, error) {
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	pubsub := m.client.Subscribe(ctx, m.channel)
	if err := pubsub.Ping(ctx); err != nil {
		_ = pubsub.Close()
		return nil, nil, fmt.Errorf("redis-syncmap: map %q: reach subscription to %q: %w", m.name, m.channel, err)
	}
	events := pubsub.ChannelWithSubscriptions()
	if err := m.awaitSubscription(ctx, events); err != nil {
		_ = pubsub.Close()
		return nil, nil, err
	}
	if m.persist != syncmap.PersistNone {
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
func (m *syncMap) awaitSubscription(ctx context.Context, events <-chan interface{}) error {
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("redis-syncmap: map %q: confirm subscription to %q: %w", m.name, m.channel, ctx.Err())
		case event, ok := <-events:
			if !ok {
				return fmt.Errorf("redis-syncmap: map %q: subscription to %q closed before confirmation", m.name, m.channel)
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

// closeSubscription closes the current connection. It is safe before start.
func (m *syncMap) closeSubscription() error {
	m.mu.Lock()
	pubsub := m.pubsub
	m.mu.Unlock()
	if pubsub == nil {
		return nil
	}
	if err := pubsub.Close(); err != nil && !errors.Is(err, redis.ErrClosed) {
		return err
	}
	return nil
}

// serve is the long-lived subscription loop. It ends when the managed task is
// cancelled or the connection closes, and closes done either way so Destroy can
// wait for it.
func (m *syncMap) serve(taskCtx context.Context) {
	defer close(m.done)

	m.mu.Lock()
	events := m.events
	m.mu.Unlock()
	if events == nil {
		return
	}

	for {
		select {
		case <-taskCtx.Done():
			return
		case event, ok := <-events:
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
}

// apply folds one notification into the local map. A malformed message, an
// unknown operation, or a transform failure is logged and skipped: storing a
// partially decoded value would leave the local map in a state no reader can
// interpret.
func (m *syncMap) apply(payload string) {
	var message broadcastMessage
	if err := json.Unmarshal([]byte(payload), &message); err != nil {
		m.warn("redis-syncmap: discarding undecodable change notification", "map", m.name)
		return
	}
	if message.Origin == m.origin {
		return
	}
	switch message.Op {
	case operationSet:
		if message.Key == "" {
			m.warn("redis-syncmap: discarding set notification without a key", "map", m.name)
			return
		}
		value, err := m.project(message.Key, message.Value)
		if err != nil {
			m.warn("redis-syncmap: discarding notification whose value the transform rejected", "map", m.name, "key", message.Key)
			return
		}
		m.store.Set(message.Key, value)
	case operationDelete:
		m.store.Delete(message.Key)
	case operationClear:
		m.store.Clear()
	default:
		m.warn("redis-syncmap: discarding unknown change notification", "map", m.name, "op", message.Op)
	}
}

// project runs the transform over a private copy of the serialized value. The
// copy matters because the transform may retain the bytes while the message
// buffer that produced them is reused.
func (m *syncMap) project(key string, raw []byte) (any, error) {
	copied := make([]byte, len(raw))
	copy(copied, raw)
	if m.transform == nil {
		return json.RawMessage(copied), nil
	}
	return m.transform(key, json.RawMessage(copied))
}

// loadSnapshot replaces the local map with the persisted collection. Entries
// the transform rejects are skipped rather than stored raw.
func (m *syncMap) loadSnapshot(ctx context.Context) error {
	data, err := m.client.HGetAll(ctx, m.hashKey).Result()
	if err != nil {
		return fmt.Errorf("redis-syncmap: map %q: load snapshot from %q: %w", m.name, m.hashKey, err)
	}
	m.replace(data)
	if m.afterSnapshot != nil {
		m.afterSnapshot()
	}
	return nil
}

func (m *syncMap) replace(data map[string]string) {
	m.store.Clear()
	for key, raw := range data {
		value, err := m.project(key, []byte(raw))
		if err != nil {
			m.warn("redis-syncmap: skipping snapshot entry whose value the transform rejected", "map", m.name, "key", key)
			continue
		}
		m.store.Set(key, value)
	}
}

// Set serializes, transforms, persists, stores locally, and broadcasts, in
// that order. See syncmap.Map for the error contract each step implies.
func (m *syncMap) Set(ctx context.Context, key string, value any) error {
	if key == "" {
		return syncmap.ErrInvalidKey
	}
	if ctx == nil {
		ctx = context.Background()
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("redis-syncmap: map %q: marshal key %q: %w", m.name, key, err)
	}
	transformed, err := m.project(key, raw)
	if err != nil {
		return fmt.Errorf("%w: map %q key %q: %w", syncmap.ErrTransform, m.name, key, err)
	}
	if m.persist != syncmap.PersistNone {
		if err := m.persistSet(ctx, key, raw); err != nil {
			return err
		}
	}
	m.store.Set(key, transformed)
	if err := m.publish(ctx, broadcastMessage{Op: operationSet, Key: key, Value: raw}); err != nil {
		return fmt.Errorf("%w: map %q key %q: local state was already updated: %w", syncmap.ErrBroadcast, m.name, key, err)
	}
	return nil
}

// persistSet stores one entry and refreshes the collection's TTL in one
// transaction. The hash has one expiry for every entry, so any write extends
// the lifetime of all of them; there is no per-entry expiry to offer here.
func (m *syncMap) persistSet(ctx context.Context, key string, raw []byte) error {
	_, err := m.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.HSet(ctx, m.hashKey, key, raw)
		if m.ttl > 0 {
			pipe.Expire(ctx, m.hashKey, m.ttl)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%w: map %q key %q: %w", syncmap.ErrPersist, m.name, key, err)
	}
	return nil
}

// Delete removes one entry. Persistence and the local delete happen before the
// notification, matching Set's ordering and error contract.
func (m *syncMap) Delete(ctx context.Context, key string) error {
	if key == "" {
		return syncmap.ErrInvalidKey
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if m.persist != syncmap.PersistNone {
		if err := m.client.HDel(ctx, m.hashKey, key).Err(); err != nil {
			return fmt.Errorf("%w: map %q key %q: %w", syncmap.ErrPersist, m.name, key, err)
		}
	}
	m.store.Delete(key)
	if err := m.publish(ctx, broadcastMessage{Op: operationDelete, Key: key}); err != nil {
		return fmt.Errorf("%w: map %q key %q: local state was already updated: %w", syncmap.ErrBroadcast, m.name, key, err)
	}
	return nil
}

// Clear removes every entry. Persistence and the local clear happen before the
// notification, matching Set's ordering and error contract.
func (m *syncMap) Clear(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if m.persist != syncmap.PersistNone {
		if err := m.client.Del(ctx, m.hashKey).Err(); err != nil {
			return fmt.Errorf("%w: map %q: %w", syncmap.ErrPersist, m.name, err)
		}
	}
	m.store.Clear()
	if err := m.publish(ctx, broadcastMessage{Op: operationClear}); err != nil {
		return fmt.Errorf("%w: map %q: local state was already updated: %w", syncmap.ErrBroadcast, m.name, err)
	}
	return nil
}

// Load returns the locally stored value.
func (m *syncMap) Load(key string) (any, bool) { return m.store.Get(key) }

// Range iterates the locally stored entries.
func (m *syncMap) Range(f func(key string, value any) bool) { m.store.Range(f) }

// Resync reloads the persisted snapshot. PersistNone has no snapshot, so the
// call reports that instead of pretending to recover missed messages.
func (m *syncMap) Resync(ctx context.Context) error {
	if m.persist == syncmap.PersistNone {
		return syncmap.ErrPersistRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return m.loadSnapshot(ctx)
}

// publish broadcasts one notification stamped with this map's identity.
func (m *syncMap) publish(ctx context.Context, message broadcastMessage) error {
	message.Origin = m.origin
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return m.client.Publish(ctx, m.channel, payload).Err()
}

// Destroy closes the subscription and waits for the loop to leave. It is
// idempotent and safe before start, after a failed start, and more than once.
func (m *syncMap) Destroy() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.destroyed {
		m.mu.Unlock()
		return nil
	}
	m.destroyed = true
	pubsub := m.pubsub
	serving := m.serving
	m.mu.Unlock()

	var failures []error
	if pubsub != nil {
		if err := pubsub.Close(); err != nil && !errors.Is(err, redis.ErrClosed) {
			failures = append(failures, fmt.Errorf("redis-syncmap: map %q: close subscription: %w", m.name, err))
		}
	}
	if serving {
		select {
		case <-m.done:
		case <-time.After(destroyWaitTimeout):
			failures = append(failures, fmt.Errorf("redis-syncmap: map %q: subscription did not stop within %s", m.name, destroyWaitTimeout))
		}
	}
	return errors.Join(failures...)
}

// warn logs through the plugin logger when one is available. A map built
// without an XBC lifecycle (a unit test) has no logger, and logging must not
// become a dependency of reading a message.
func (m *syncMap) warn(message string, kv ...any) {
	if m.log == nil {
		return
	}
	m.log.Warn(message, kv...)
}
