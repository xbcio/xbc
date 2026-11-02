// Package redissyncmap provides a configured XBC plugin that exports a
// syncmap.Factory over one Redis client. Every map it creates keeps its entries
// in local memory and synchronizes changes with the other replicas that share
// the configured namespace. Importing this package is side-effect free; prefer
// explicit composition with this package's Bundle().
//
// # Usage
//
// Configure a Redis instance and one section that selects it:
//
//	plugins:
//	  redis:
//	    default: { addr: "127.0.0.1:6379" }
//	  redis-syncmap:
//	    default:
//	      redis_instance: default
//	      namespace: xbc
//	      persist: ttl
//	      ttl: 1h
//	      max_size: 1024
//
// A consumer resolves the exported contract and creates every map it needs
// while its own factory runs, before the factory starts:
//
//	var factoryInput = plugin.RefTo[syncmap.Factory](redissyncmap.Key)
//
//	var cacheDefinition = plugin.Define(
//		"rule-cache",
//		func(ctx plugin.BuildContext) (*ruleCache, error) {
//			rules, err := factoryInput.Get(ctx).Value.Map("rules",
//				syncmap.WithTransform(func(_ string, raw json.RawMessage) (any, error) {
//					var rule Rule
//					if err := json.Unmarshal(raw, &rule); err != nil {
//						return nil, err
//					}
//					return rule.Compile()
//				}),
//			)
//			if err != nil {
//				return nil, err
//			}
//			return &ruleCache{rules: rules}, nil
//		},
//		plugin.Options[*ruleCache]{Inputs: plugin.Inputs(factoryInput)},
//	)
//
// The composition root includes both redis.Bundle() and
// redis-syncmap.Bundle(). The factory owns every map it creates and destroys
// them during Drain and Stop; an individual syncmap.Map.Destroy is available
// but rarely needed.
//
// # Persistence
//
// persist: none (the default) broadcasts changes only. A map starts empty and
// cannot recover notifications published while its subscription was down;
// Resync reports syncmap.ErrPersistRequired. persist: ttl stores the whole map
// as one Redis hash and applies ttl to that hash. Redis has one expiry per
// hash, so every successful write refreshes the lifetime of every entry in the
// map; there is no per-entry TTL, and this plugin does not pretend otherwise.
//
// max_size bounds each map's local cache with LRU eviction. Eviction is local:
// it never deletes from Redis and never broadcasts, so the next Load of an
// evicted key reports absence until a later write, notification, or Resync
// brings the entry back. Resync replaces the local map with the persisted
// snapshot; a write concurrent with its snapshot read may be overwritten
// locally, so callers that must not lose one serialize the two themselves.
//
// # Lifecycle
//
// Init records the plugin logger. Start subscribes every map and launches its
// listener as a managed task; each subscription is confirmed before that map's
// snapshot is read, so a change published during startup is buffered rather
// than lost. Drain and Stop both close the factory to new maps and destroy the
// existing ones, and both are idempotent, including after a failed Init or
// Start.
package redissyncmap
