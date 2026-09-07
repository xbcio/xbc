// Package casbinredis provides an XBC Casbin WatcherFactory backed by Redis
// Pub/Sub. Each Factory.Open call creates a private publisher/subscriber pair;
// it neither consumes nor closes clients exported by integrations/redis.
// Importing this package has no registration side effects.
//
// # Usage
//
// Compose the Redis watcher factory with the base Casbin integration:
//
//	func applicationBundle() plugin.Bundle {
//		return plugin.CombineBundles(
//			casbingorm.Bundle(),
//			casbinredis.Bundle(),
//			casbin.Bundle(),
//		)
//	}
//
// Configure a named watcher instance and select that exact exporter from the
// base Casbin plugin:
//
//	plugins:
//	  casbin-redis:
//	    authorization:
//	      mode: standalone
//	      addrs: [redis.internal:6379]
//	      password: ${CASBIN_REDIS_PASSWORD}
//	      channel: /casbin
//	      ignore_self: true
//	  casbin:
//	    adapter:
//	      plugin: casbin-gorm
//	      instance: authorization
//	    watcher:
//	      plugin: casbin-redis
//	      instance: authorization
//
// The base Casbin plugin requires a persistent AdapterProvider whenever a
// watcher is configured; casbin-gorm above is one such provider. Cluster mode
// accepts one or more seed addresses and requires db: 0. The Factory is
// connection-free; the base Casbin plugin calls Open for a live
// enforcer and owns the returned watcher's exactly-once Close. Applications
// constructing a watcher directly assume the same ownership obligation:
//
//	factory, err := casbinredis.New(casbinredis.DefaultConfig())
//	if err != nil {
//		return err
//	}
//	watcher, err := factory.Open(enforcer)
//	if err != nil {
//		return err
//	}
//	defer watcher.Close()
package casbinredis
