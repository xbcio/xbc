// Package distributed provides coordination primitives whose state lives in
// Redis and is shared by every process that uses the same prefix: a
// synchronized string-keyed map, a cluster-wide semaphore, and a cluster-wide
// rate limiter.
//
// The package is an ordinary framework-free library: it depends on the
// standard library and one Redis client, and it never imports any package of
// the framework it is developed beside. That is what lets a component use a
// synchronized cache or a distributed limit without inheriting an application
// runtime, a plugin Definition, or a transport. When an application wants an
// interface over a primitive, it declares one at the call site; the Go
// convention is to consume a concrete type and accept an interface only where
// a boundary needs it, so this package exports no interface of its own.
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
// # Semaphores
//
// A Semaphore bounds how many executions of one named operation run at once
// across the cluster. Run acquires a slot, renews its lease in the background,
// and releases it when the callback returns; Reset discards expired slots and
// leaves live ones alone, which is safe during a rolling restart.
//
// The slots of one name live in the Redis sorted set distributed:sem:<name>
// (the prefix is configurable). Its members are per-execution tokens scored by
// their lease expiry in Unix milliseconds. Acquisition discards expired tokens
// before counting, a running execution renews its token every lease/3, and the
// key is set to expire after twice the lease, so a process that dies frees its
// slots when the leases lapse instead of blocking the name forever. When
// renewal finds the token gone, or Redis could not renew it three times in a
// row, the callback's context is cancelled: the slot no longer protects the
// work that was admitted under it. Run reports ErrLimitReached when every slot
// is taken, and a key holding a Redis type other than a sorted set is deleted
// and recreated by both acquisition and Reset.
//
// # Rate limiters
//
// A RateLimiter paces one named operation across the cluster at limit events
// per per. It is the cluster-wide counterpart of a process-local limiter: the
// process-local limiter keeps a schedule per replica, so N replicas each admit
// limit events per window; a RateLimiter keeps one schedule in Redis, so the
// replicas share the quota.
//
// The algorithm is GCRA (the generic cell rate algorithm), a leaky bucket
// viewed as a meter. One value per name is a theoretical arrival time (TAT) in
// Unix milliseconds. The emission interval is per/limit, and the burst
// tolerance is one emission interval short of the window. A request arriving
// at t may run when t >= TAT - tolerance; a reservation then moves TAT to
// max(TAT, t) + interval. The first limit reservations in a window therefore
// run at once, and every later reservation is scheduled one interval after the
// previous one. A burst cannot exceed limit, the long-term rate cannot exceed
// limit per per, and there is no fixed-window boundary at which twice the
// quota can be admitted back to back.
//
// The clock is the Redis server's TIME, read inside the same Lua script that
// reads and writes TAT. Every replica schedules against one clock, so clock
// skew between hosts cannot distort the rate, and concurrent reservations are
// serialized by Redis: the script is one atomic read-modify-write, not a GET
// followed by a SET that two replicas could interleave.
//
// per/limit is rarely a whole number of milliseconds. The stored TAT keeps its
// fractional millisecond part instead of being rounded on every step, so the
// rounding error does not accumulate into a rate error: 3 per 10ms is an
// emission interval of 10/3ms, and the schedule stays at 300 events per second
// rather than drifting toward 333 or 250.
//
// Reserve consumes a slot and returns how long the caller must wait before it
// may run; zero means run now. It consumes even when the delay is positive, so
// a later caller observes the reservation. Wait is Reserve followed by a
// sleep, and returns the caller's context error if the context ends first; the
// slot is not returned, because a reservation is the consumption. Allow
// consumes a slot only when one is available now and reports false otherwise,
// leaving the schedule untouched; it does not report a suggested delay, so a
// caller that must retry applies its own backoff. Use Reserve when the caller
// wants to schedule work itself, Wait when it can simply block, and Allow when
// it has other work to do on refusal.
//
// The key expires after twice per and every reservation refreshes that expiry,
// so an abandoned name does not leak. The trade-off is that reservation debt
// not touched for twice per is forgotten; the bucket then refills early.
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
// distributed:map:rules and the channel distributed:map:rules:events. One
// semaphore named "report.render" uses the sorted set
// distributed:sem:report.render. One rate limiter named "report.render" uses
// the string distributed:rate:report.render. Two deployments that share a
// Redis instance share state when they share a prefix; give each logical
// cluster its own prefix.
package distributed
