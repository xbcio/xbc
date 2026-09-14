// Package gracefulshutdown exposes a programmatic controller and an optional
// protected HTTP endpoint that request XBC's existing process shutdown path.
//
// # Usage
//
// Definition() returns this package's primary Definition: Key produces
// *Controller. Bundle() deliberately selects two independent runtime units:
// that Controller and HTTPKey, which produces the Web route contributor. The
// route contributor has its own Key, configuration, and contract, and receives
// Controller through plugin.RefTo; no value is published from a lifecycle hook.
// Keeping both Definitions in one Bundle makes the optional HTTP adapter easy
// to select without conflating it with the programmatic controller.
//
// A plugin that needs to trigger a clean process stop declares the same typed
// construction dependency:
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
// Include gracefulshutdown.Bundle() and a plugins.gracefulshutdown section to
// activate both Definitions. The HTTP endpoint remains disabled unless
// http.enabled is true. When enabled, access control is determined by the
// application's authentication policy.
//
// An accepted request cancels every plugin lifecycle context and lets core
// perform its normal bounded reverse-order Stop sequence, including HTTP
// connection draining. Controller.Stop detaches later callers. Bundle and
// ordinary imports are side-effect free; xbc.Run applications may opt into the
// leaf autoload adapter.
package gracefulshutdown
