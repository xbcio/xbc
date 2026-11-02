package syncmap

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// PersistMode selects what a map keeps in the backing store.
type PersistMode string

const (
	// PersistNone broadcasts changes without persisting them. A map created in
	// this mode starts empty, and a subscription that was down while a change
	// was published cannot recover it: there is no snapshot to resynchronize
	// from. Resync reports ErrPersistRequired in this mode.
	PersistNone PersistMode = "none"

	// PersistTTL stores every entry in the backing store and applies the
	// map's TTL to the stored collection. The TTL belongs to the collection,
	// not to individual entries: one write refreshes the expiry of every entry
	// the collection holds. See Map.Set and Factory.Map.
	PersistTTL PersistMode = "ttl"
)

// TransformFunc converts the serialized value of one entry into the value Load
// and Range return. value is the entry's JSON bytes exactly as they were
// written, and the implementation hands over a copy the transform may retain;
// the transform is responsible for decoding it with its own tags -- for
// example json.Unmarshal or sonic.Unmarshal.
//
// The contract deliberately does not normalize the bytes into map[string]any
// before calling the transform. A generic decode loses distinctions the
// caller's own type has (integer fields become float64), and asking every
// transform to repair that normalization is what turns a typed value into a
// zero value at the call site.
//
// A transform returning an error rejects the entry: Set returns an error
// wrapping ErrTransform and stores nothing locally, persists nothing, and
// broadcasts nothing; an entry arriving from the backing store is skipped and
// never reaches the local map. A failed transform therefore never leaves a
// partially decoded value behind.
type TransformFunc func(key string, value json.RawMessage) (any, error)

// Options is the resolved per-map configuration. A Factory applies its
// configured defaults, then the Option values passed to Factory.Map.
//
// The zero Transform keeps the entry's JSON bytes as its value: Load and
// Range then return json.RawMessage. The zero Persist is PersistNone. The zero
// TTL means no expiry in PersistTTL mode, which Factory.Map rejects: that mode
// requires a positive TTL. The zero MaxSize means unlimited; a positive size
// bounds the local cache with LRU eviction.
type Options struct {
	Transform TransformFunc
	Persist   PersistMode
	TTL       time.Duration
	MaxSize   int
}

// Option adjusts one map's Options.
type Option func(*Options)

// WithTransform installs the transform that projects serialized bytes into the
// caller's own value type.
func WithTransform(transform TransformFunc) Option {
	return func(options *Options) { options.Transform = transform }
}

// WithPersist overrides the factory's default persistence mode.
func WithPersist(mode PersistMode) Option {
	return func(options *Options) { options.Persist = mode }
}

// WithTTL overrides the factory's default TTL. It is meaningful only in
// PersistTTL mode, where it applies to the whole stored collection.
func WithTTL(ttl time.Duration) Option {
	return func(options *Options) { options.TTL = ttl }
}

// WithMaxSize overrides the factory's default local cache bound. A positive
// size enables LRU eviction; zero means unlimited. A negative size is rejected
// as ErrInvalidOption.
func WithMaxSize(size int) Option {
	return func(options *Options) { options.MaxSize = size }
}

// Factory creates and owns named maps over one backing store. A Factory is
// obtained from a plugin's exported contract; the implementation module owns
// the concrete type.
//
// Map must be called before the factory starts, which in an XBC application
// means from a plugin factory or an Init hook: the implementation launches each
// map's subscription as a managed Start task, so a factory that has already
// started cannot adopt another map and reports ErrStarted. A destroyed factory
// reports ErrClosed. Map names identify one map within the factory's namespace:
// a repeated name reports ErrDuplicate rather than silently returning the
// first map, because two consumers asking for the same name usually mean two
// consumers expecting independent state.
type Factory interface {
	Map(name string, options ...Option) (Map, error)
}

// Map is one synchronized string-keyed map.
//
// Keys are strings by construction. The earlier design serialized arbitrary
// keys with their Go type name and rebuilt them on the receiving side through
// an untyped JSON decode; numbers came back as float64, so an int key written
// by one process did not match the key its peers had stored. Restricting the
// key to string removes that class of mismatch instead of documenting it away.
//
// # Write semantics
//
// Set, Delete, and Clear follow one order: serialize the value, run the
// transform locally, persist (in PersistTTL mode), change the local map, then
// broadcast. The consequences are deliberate:
//
//   - A transform failure returns an error wrapping ErrTransform and changes
//     nothing: no local entry, no persisted entry, no message.
//   - A persistence failure returns an error wrapping ErrPersist and changes
//     nothing locally. The local map and the backing store stay consistent.
//   - A broadcast failure returns an error wrapping ErrBroadcast after the
//     local map and, in PersistTTL mode, the backing store have already
//     changed. The write is not rolled back: rolling back a successful
//     persistence would risk overwriting a newer value written by a peer, and
//     peers that missed the message recover the state from the backing store
//     with Resync.
//
// A message carries the identity of the map instance that published it, and an
// instance drops its own messages before any transform or store runs. Without
// that identity every write would be applied twice locally -- once by the
// writer and once by the writer's own subscription -- and a counting transform
// would observe both.
//
// Load, Range, and a successful Set observe the transformed local value.
// Without a transform the value is the entry's JSON bytes as json.RawMessage;
// callers must treat that byte slice as read-only.
type Map interface {
	// Set stores value under key and propagates it. See the write semantics
	// above. ctx bounds every backing-store round trip the call performs.
	Set(ctx context.Context, key string, value any) error

	// Load returns the transformed value stored under key. The boolean is
	// false when the key is absent, including when a local LRU has evicted it.
	Load(key string) (value any, ok bool)

	// Delete removes key and propagates the removal. It follows the same
	// ordering and error rules as Set.
	Delete(ctx context.Context, key string) error

	// Clear removes every entry and propagates the removal. It follows the
	// same ordering and error rules as Set.
	Clear(ctx context.Context) error

	// Range calls f for each entry in local recency order, most recently used
	// first, until f returns false. The values are the transformed values.
	// Range iterates a snapshot of the keys taken when it was called; entries
	// added or removed by f are not observed by this call.
	Range(f func(key string, value any) bool)

	// Resync reloads the persisted snapshot and replaces the local map with it.
	// It exists for the gap a subscription cannot cover: messages published
	// while a subscriber was disconnected are gone, and only the persisted
	// state can restore them. In PersistNone mode there is no snapshot and
	// Resync reports ErrPersistRequired; missed NoPersist messages are
	// unrecoverable by design.
	//
	// Resync is a recovery operation, not a consistency mechanism: a write
	// concurrent with its snapshot read may be overwritten locally. Callers
	// that must not lose concurrent writes serialize the two themselves.
	Resync(ctx context.Context) error

	// Destroy stops this map's subscription and releases it. It is
	// idempotent, safe before the map started, and does not delete persisted
	// data or notify peers. The owning Factory also destroys every map it
	// created when it drains or stops; a caller rarely needs to call this.
	Destroy() error
}

var (
	// ErrClosed reports that the factory has been drained or stopped.
	ErrClosed = errors.New("syncmap: factory is closed")
	// ErrStarted reports that the factory has already started and cannot adopt
	// another map.
	ErrStarted = errors.New("syncmap: factory has already started")
	// ErrDuplicate reports that a map with this name already exists in the
	// factory.
	ErrDuplicate = errors.New("syncmap: map already exists")
	// ErrInvalidName reports an empty or malformed map name.
	ErrInvalidName = errors.New("syncmap: invalid map name")
	// ErrInvalidKey reports an empty key.
	ErrInvalidKey = errors.New("syncmap: key cannot be empty")
	// ErrInvalidOption reports an option combination the implementation
	// refuses, such as a negative MaxSize or PersistTTL without a positive TTL.
	ErrInvalidOption = errors.New("syncmap: invalid option")
	// ErrTransform reports that the map's TransformFunc rejected an entry.
	ErrTransform = errors.New("syncmap: transform failed")
	// ErrPersist reports that the backing store rejected a write. The local
	// map was not changed.
	ErrPersist = errors.New("syncmap: persistence failed")
	// ErrBroadcast reports that the change notification could not be
	// published. The local map, and in PersistTTL mode the backing store, were
	// already changed.
	ErrBroadcast = errors.New("syncmap: broadcast failed")
	// ErrPersistRequired reports Resync on a PersistNone map, which has no
	// snapshot to reload.
	ErrPersistRequired = errors.New("syncmap: resync requires persistence")
)
