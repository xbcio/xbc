// Package syncmap defines the protocol-neutral contract for a memory map kept
// in step across process replicas.
//
// It is a contract module: it owns no Definition, no Config, and no Bundle. It
// publishes only the vocabulary a backend implements and an application calls,
// so a component that needs its caches synchronized across replicas never has
// to depend on the backend that happens to provide the synchronization.
//
// # What the contract promises
//
// A Factory creates named maps over one backing store. Every Map holds its
// entries in local memory and propagates changes through the store, so a Load
// or Range never performs I/O; only Set, Delete, Clear, and Resync touch the
// backing store. Keys are strings. Values are opaque to the map: it serializes
// them as JSON, and an optional TransformFunc turns those bytes back into the
// caller's own type for local reads.
//
// A change notification carries the identity of the map instance that
// published it, so an instance drops its own messages instead of applying a
// write twice. Persistence, when enabled, is written before the notification,
// so a peer that resynchronizes observes at least the values the snapshot
// covered. The subscription is established and confirmed before the snapshot
// is read, so a message published while the map is starting is delivered to the
// subscription rather than falling into an unobserved window.
//
// # What the contract deliberately does not promise
//
// Entries are not versioned. Two replicas writing the same key concurrently
// converge on the notification order, not on a total order or a last-write-win
// timestamp; a caller that needs one must serialize the writers or store a
// version in its own value. Ordering between a snapshot and messages is only as
// strong as the backend's own write-before-publish order.
//
// A PersistNone map cannot recover a message missed while its subscription was
// down. Resync reports ErrPersistRequired rather than pretending otherwise.
//
// In PersistTTL mode the TTL belongs to the stored collection, not to one entry:
// any successful write refreshes the expiry of every entry the collection
// holds. The backing store chosen here does not offer a per-entry expiry, and
// the contract does not pretend it does.
//
// # Usage
//
// An application resolves the Factory from a plugin's export, creates every map
// it needs before the owning plugin starts, and uses the Map from its own
// components:
//
//	var factoryInput = plugin.RefTo[syncmap.Factory](redisSyncMapKey)
//
//	type ruleCache struct {
//		rules syncmap.Map
//	}
//
//	func newRuleCache(ctx plugin.BuildContext) (*ruleCache, error) {
//		rules, err := factoryInput.Get(ctx).Value.Map("rules",
//			syncmap.WithTransform(func(key string, raw json.RawMessage) (any, error) {
//				var rule Rule
//				if err := json.Unmarshal(raw, &rule); err != nil {
//					return nil, err
//				}
//				return rule.Compile()
//			}),
//			syncmap.WithPersist(syncmap.PersistTTL),
//			syncmap.WithTTL(time.Hour),
//			syncmap.WithMaxSize(1024),
//		)
//		if err != nil {
//			return nil, err
//		}
//		return &ruleCache{rules: rules}, nil
//	}
//
//	func (cache *ruleCache) refresh(ctx context.Context) error { return cache.rules.Resync(ctx) }
package syncmap
