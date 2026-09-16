// Package redis provides a configured XBC plugin whose primary value is one
// standalone *redis.Client. Importing this package is side-effect free. Prefer
// explicit composition with redis.Bundle(); executables that deliberately use
// process-wide autoload may blank-import the autoload subpackage.
//
// # Usage
//
// Each named section below plugins.redis constructs one *redis.Client. A
// consumer declares and reads the exact typed input token during construction:
//
//	var cacheClient = plugin.RefToInstance[*goredis.Client](redis.Key, "cache")
//
//	type cache struct {
//		client *goredis.Client
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
// lifecycle adapter. Keep credentials in secret-backed configuration and use
// this standalone connection only across a trusted network boundary or a
// separately secured tunnel.
//
// # Readiness
//
// Bundle also selects HealthKey, a second Definition that exports
// health.Contributor and reports one readiness check per configured instance. It
// is a separate Definition because this plugin's primary value is the
// third-party *redis.Client, which cannot be given a HealthChecks method. The
// probe is inert unless the application also selects the health capability
// Bundle; when it is selected, a configured instance appears in the aggregate
// readiness report as "redis-health" or "redis-health/<instance>" with no change
// at the composition root.
package redis
