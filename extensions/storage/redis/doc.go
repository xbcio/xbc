// Package redis provides a configured XBC plugin whose primary value owns one
// Redis client. Which topology that client addresses -- a standalone server, a
// sentinel-managed master, or a cluster -- is fixed by the instance's mode.
// Importing this package is side-effect free. Prefer explicit composition with
// redis.Bundle(); executables that deliberately use process-wide autoload may
// blank-import the autoload subpackage.
//
// # Usage
//
// Each named section below plugins.redis constructs one client. Consumers
// resolve the topology-neutral goredis.UniversalClient contract, so one
// consumer works against every mode; a consumer that needs the plugin's
// concrete primary value declares its Ref over this package's *Client instead:
//
//	var cacheClient = plugin.RefToInstance[goredis.UniversalClient](redis.Key, "cache")
//
//	type cache struct {
//		client goredis.UniversalClient
//	}
//
//	var cacheDefinition = plugin.Define(
//		"cache",
//		func(ctx plugin.BuildContext) (*cache, error) {
//			return &cache{client: cacheClient.Get(ctx).Value}, nil
//		},
//		plugin.Options[*cache]{Inputs: plugin.Inputs(cacheClient)},
//	)
//
//	func Bundle() plugin.Bundle {
//		return plugin.BundleOf(cacheDefinition)
//	}
//
// The composition root includes both redis.Bundle() and the consumer's Bundle.
// The integration owns the go-redis client and its pool. It checks connectivity
// during construction by default; disabling ping deliberately makes connection
// establishment lazy. XBC closes the client through a typed, idempotent
// lifecycle adapter. Keep credentials in secret-backed configuration.
//
// # TLS
//
// Each instance can speak TLS to its Redis deployment:
//
//	plugins:
//	  redis:
//	    cache:
//	      addr: "redis.internal:6379"
//	      tls:
//	        ca_file: /etc/xbc/redis/ca.pem
//	        cert_file: /etc/xbc/redis/client.pem   # optional, for mTLS
//	        key_file: /etc/xbc/redis/client-key.pem
//	        server_name: redis.internal
//	        min_version: "1.2"
//
// The section is off by default and the connection stays in cleartext until one
// of ca_file, cert_file, key_file, server_name, or insecure_skip_verify is set.
// ca_file replaces the system roots; cert_file and key_file must be configured
// together. min_version accepts "1.2" or "1.3" only. The material is read when
// the instance is constructed, so an unreadable file fails startup rather than
// the first command. insecure_skip_verify disables certificate verification: it
// exists for development endpoints and must not be used in production, where an
// active attacker would reduce TLS to unauthenticated encryption.
//
// # Readiness
//
// Bundle also selects HealthKey, a second Definition that exports
// health.Contributor and reports one readiness check per configured instance. It
// is a separate Definition because it aggregates: a contributor carried by the
// client Definition would answer only for the single instance its primary
// represents. The probe is inert unless the application also selects the health
// capability Bundle; when it is selected, a configured instance appears in the
// aggregate readiness report as "redis-health" or "redis-health/<instance>"
// with no change at the composition root.
//
// # Leases
//
// Bundle also selects LeaseKey, a second Definition that exports lease.Locker
// over exactly one configured instance:
//
//	plugins:
//	  redis:
//	    default: { addr: "127.0.0.1:6379" }
//	  redis-lease:
//	    instance: "default"
//
// The lease capability is inert unless plugins.redis-lease is configured, and
// it names its instance rather than collecting every client, because a lease
// belongs to one connection. NewLocker is the same implementation without the
// plugin graph, for composition roots that must decide placement before a plan
// exists; it accepts any topology's client, this plugin's included.
package redis
