// Package distributed provides a string-keyed map whose entries stay in step
// across process replicas, with Redis as the only backend.
//
// The package is an ordinary framework-free library: it depends on the
// standard library and one Redis client, and it never imports any package of
// the framework it is developed beside. That is what lets a component use a
// synchronized cache without inheriting an application runtime, a plugin
// Definition, or a transport. When an application wants an interface over the
// map, it declares one at the call site; the Go convention is to consume a
// concrete type and accept an interface only where a boundary needs it, so
// this package exports no interface of its own.
//
// # What the map promises
//
// Every entry is held in local memory, so Load, Range, and Len never perform
// I/O; only Set, Delete, Clear, and Resync touch the backing store. Keys are
// strings by construction. Values are opaque to the map: Set serializes them
// as JSON and Decode turns those bytes back into V on every read path, local
// and remote alike, so a writer and its peers observe the same value shape.
//
// A change notification carries the identity of the map instance that
// published it, and an instance drops its own messages before decoding. A
// subscription is established and confirmed before the snapshot is read, so a
// message published while the map is starting is buffered for the listener
// instead of falling into an unobserved window.
//
// There is no process-wide registry. Two NewMap calls with the same name and
// prefix create two independent instances, each with its own identity; both
// subscribe to the same channel, each drops only its own publications, and
// both therefore observe the other's writes. A caller that wants one shared
// instance per process holds one value itself.
//
// # What the map deliberately does not promise
//
// Entries are not versioned. Two replicas writing the same key concurrently
// converge on the notification order, not on a total order or a
// last-write-wins timestamp; a caller that needs one must serialize the
// writers or store a version in its own value.
//
// In PersistNone mode the map starts empty and cannot recover a message
// published while its subscription was down. Resync reports
// ErrPersistRequired rather than pretending otherwise.
//
// In PersistTTL mode the TTL belongs to the stored collection, not to one
// entry: any successful write refreshes the expiry of every entry the
// collection holds, because Redis has one expiry per hash. MaxSize bounds the
// local cache with LRU eviction; eviction is local only and never deletes from
// Redis or broadcasts.
//
// # Usage in an XBC application
//
// A plugin declares the Redis client as an input and creates the map in its
// Start hook, where it has a lifecycle context, then closes it in Stop:
//
//	const redisPluginKey plugin.Key = "redis"
//
//	var redisInput = plugin.RefToInstance[goredis.UniversalClient](redisPluginKey, plugin.DefaultInstance)
//
//	type ruleCache struct {
//		client goredis.UniversalClient
//		rules  *distributed.Map[Rule]
//	}
//
//	func newRuleCache(ctx plugin.BuildContext) (*ruleCache, error) {
//		return &ruleCache{client: redisInput.Get(ctx).Value}, nil
//	}
//
//	func (cache *ruleCache) Init(ctx *plugin.Context) error { return nil }
//
//	func (cache *ruleCache) Start(ctx *plugin.Context) error {
//		rules, err := distributed.NewMap[Rule](ctx, cache.client, "rules", distributed.MapOptions[Rule]{
//			Persist: distributed.PersistTTL,
//			TTL:     time.Hour,
//			MaxSize: 1024,
//		})
//		if err != nil {
//			return err
//		}
//		cache.rules = rules
//		return nil
//	}
//
//	func (cache *ruleCache) Stop(context.Context) error { return cache.rules.Close() }
//
// Because the package owns no plugin surface, the same code is available to a
// plain service that constructs its own Redis client.
//
// # Keys
//
// With the default prefix, one map named "rules" uses the Redis hash
// distributed:map:rules and the channel distributed:map:rules:events. Two
// deployments that share a Redis instance share state when they share a
// prefix; give each logical cluster its own prefix.
package distributed
