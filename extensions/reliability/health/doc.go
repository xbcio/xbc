// Package health owns XBC's protocol-neutral health-check vocabulary and the
// aggregation plugin behind every probe.
//
// This module names no transport. It answers probes programmatically through
// Plugin.Check, and a transport adapter renders the returned Report for its own
// wire format: transport/web/extensions/reliability/health serves the HTTP
// liveness and readiness endpoints. Keeping the capability here is what lets a
// dependency plugin contribute a probe without inheriting a transport's
// dependency closure, and what lets a background-only service answer health
// questions with no transport selected at all.
//
// # Usage
//
// Compose the health Bundle explicitly. The Definition consumes every Plugin
// that exports Contributor and preserves each producer Identity for diagnostic
// check names:
//
//	app, err := xbc.New(xbc.WithBundles(
//		health.Bundle(),
//		gorm.Bundle(),
//		redis.Bundle(),
//	))
//
// A dependency contributes one or more checks by exporting Contributor from its
// canonical Definition:
//
//	type DatabasePlugin struct {
//		DB *sql.DB
//	}
//
//	func (p *DatabasePlugin) HealthChecks() []health.NamedChecker {
//		return []health.NamedChecker{{
//			Name:    "primary",
//			Kind:    health.Readiness,
//			Timeout: 500 * time.Millisecond,
//			Checker: health.CheckFunc(p.DB.PingContext),
//		}}
//	}
//
//	var definition = plugin.Define(
//		"database",
//		openDatabase,
//		plugin.Options[*DatabasePlugin]{
//			Exports: plugin.Contracts(
//				plugin.ExportAs[health.Contributor](
//					func(value *DatabasePlugin) health.Contributor { return value },
//				),
//			),
//		},
//	)
//
// Contributors are injected once during construction; Plugin.Check never scans
// mutable runtime state. Each selected check runs with a bounded context, and
// the aggregate report is stable and name-sorted.
//
// A contribution with an empty Name is reported under the producing Identity
// alone, which is what distinguishes the instances of a multi-instance plugin
// from each other. extensions/storage/elasticsearch,
// extensions/storage/objectstorage, extensions/messaging/kafka,
// extensions/jobs/asynq, and extensions/coordination/raft own their primary type
// and use this direct shape.
//
// A plugin whose primary value is a third-party type cannot implement
// Contributor, because a Definition may only declare contracts its primary type
// is assignable to and a foreign type cannot be given a method. Such a plugin
// contributes through a second Definition in the same Bundle whose primary is
// its own type and which collects the values it should probe:
//
//	var clients = plugin.Collect[*goredis.Client]()
//
//	var healthDefinition = plugin.Define(
//		"redis-health",
//		func(ctx plugin.BuildContext) (*healthProbe, error) {
//			return &healthProbe{clients: clients.Get(ctx)}, nil
//		},
//		plugin.Options[*healthProbe]{
//			Activation: plugin.WhenConfigured("plugins.redis"),
//			Inputs:     plugin.Inputs(clients),
//			Exports: plugin.Contracts(
//				plugin.ExportAs[health.Contributor](
//					func(value *healthProbe) health.Contributor { return value },
//				),
//			),
//		},
//	)
//
// extensions/storage/redis and extensions/storage/gorm use this shape, so every
// configured instance is probed without any change at the composition root.
//
// Once XBC begins graceful shutdown, readiness immediately reports Down without
// calling any contributor, while liveness remains Up. Result.Error is always
// available to programmatic callers; whether it may cross a protocol boundary
// is the adapter's decision, expressed with DetailPolicy. Bundle and ordinary
// imports are side-effect free; executables using xbc.Run may opt into the leaf
// autoload adapter.
package health
