// Package gracefulshutdown is the programmatic way for application code --
// including non-plugin code merely held by a plugin -- to request XBC's
// existing, normal graceful shutdown path.
//
// SIGINT and SIGTERM are handled by the runtime itself and need no plugin:
// a signal drains the Web listener if one is selected, then unwinds every
// started plugin in reverse order within the shared shutdown budget. This
// package exists for the other trigger, a plugin or application component
// that decides on its own, from inside a request handler, a background loop,
// or a maintenance task, that the process should stop now. Controller.Request
// reuses the same unified cancellation, listener drain, and reverse-order
// plugin Stop sequence a signal uses; it never calls os.Exit on its own.
//
// # Usage
//
// Compose the Bundle explicitly and activate the Controller:
//
//	app, err := xbc.New(xbc.WithBundles(
//		gracefulshutdown.Bundle(),
//	))
//
//	plugins:
//	  gracefulshutdown: {}
//
// A plugin that needs to trigger a clean process stop declares the same typed
// construction dependency every other consumer of a Definition's primary value
// does:
//
//	var shutdownInput = plugin.RefTo[*gracefulshutdown.Controller](
//		gracefulshutdown.Key,
//	)
//
//	var definition = plugin.Define(
//		"maintenance",
//		func(ctx plugin.BuildContext) (*Maintenance, error) {
//			return &Maintenance{shutdown: shutdownInput.Get(ctx).Value}, nil
//		},
//		plugin.Options[*Maintenance]{
//			Inputs: plugin.Inputs(shutdownInput),
//		},
//	)
//
//	func (p *Maintenance) ShutdownAfterDrain() bool {
//		return p.shutdown.Request("maintenance drain complete")
//	}
//
// An accepted Request cancels every plugin lifecycle context and lets core
// perform its normal bounded reverse-order Stop sequence. Request returns true
// only for the caller whose request core accepted first; every later caller,
// and any caller holding an unbound Controller, gets false. Controller.Stop
// detaches later callers; shutdown itself stays orchestrated by core.
//
// This package deliberately offers no remote or HTTP trigger. A route that
// stops the process is a liability reachable by anyone who can reach it, not a
// convenience: there is no tier of authentication or authorization this
// package could pick on an application's behalf that would make exposing
// process control over a network boundary a safe default. An application that
// still wants an operator-facing remote trigger builds its own protected route
// and calls Controller.Request from the handler; it then owns that decision
// and its access control, instead of inheriting one made here. Bundle and
// ordinary imports are side-effect free; executables using xbc.Run may opt
// into the leaf autoload adapter.
package gracefulshutdown
