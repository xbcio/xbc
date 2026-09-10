// Package pprof exposes Go runtime profiles through XBC's Web transport.
//
// # Usage
//
// Compose Web and pprof explicitly; ordinary imports have no registration
// effects:
//
//	app, err := xbc.New(xbc.WithBundles(
//		web.Bundle(),
//		pprof.Bundle(),
//	))
//
// A plugins.pprof section activates the Definition:
//
//	plugins:
//	  pprof:
//	    enabled: true
//	    path: /debug/pprof
//
// The endpoint is disabled unless plugins.pprof exists and enabled is true.
// When enabled, access control is determined by the application's
// authentication policy. Runtime profiles can contain sensitive data and
// consume substantial CPU, so keep the endpoint on a restricted management
// network.
//
// This plugin exports web.RouteContributor but owns no standalone server; Web
// controls serving and connection-drain lifecycle. Applications using xbc.Run
// may opt into process-global composition through the leaf autoload adapter.
package pprof
