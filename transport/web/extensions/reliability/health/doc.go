// Package health owns XBC's protocol-independent health-check contracts and
// aggregation plugin.
//
// # Usage
//
// Compose the health Bundle explicitly. The Definition consumes every Plugin
// that exports Contributor and preserves each producer Identity for diagnostic
// check names:
//
//	app, err := xbc.New(xbc.WithBundles(
//		web.Bundle(),
//		health.Bundle(),
//		database.Bundle(),
//	))
//
// A dependency contributes one or more checks by exporting Contributor from
// its canonical Definition:
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
// The plugin exports web.RouteContributor and registers public liveness and
// readiness routes. Diagnostic errors are hidden by DetailNever by default to
// avoid exposing internal addresses or dependency details. Bundle and ordinary
// imports are side-effect free; executables using xbc.Run may opt into the leaf
// autoload adapter.
package health
