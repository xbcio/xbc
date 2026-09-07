// Package asynq provides an XBC-managed Redis-backed task runtime. Importing
// this package is side-effect free; applications explicitly compose Bundle or
// opt in through the autoload subpackage.
//
// # Usage
//
// Define handlers as a canonical Plugin that explicitly exports
// HandlerContributor, then compose its Bundle with asynq.Bundle:
//
//	type mailHandlers struct{}
//
//	func (*mailHandlers) TaskHandlers() []asynq.HandlerRegistration {
//		return []asynq.HandlerRegistration{{
//			Type: "mail.send",
//			Handler: asynq.HandlerFunc(func(context.Context, asynq.Task) error {
//				return nil
//			}),
//		}}
//	}
//
//	var definition = plugin.Define(
//		"mail-handlers",
//		func(plugin.BuildContext) (*mailHandlers, error) { return &mailHandlers{}, nil },
//		plugin.Options[*mailHandlers]{
//			Exports: plugin.Contracts(
//				plugin.ExportAs[asynq.HandlerContributor](
//					func(handlers *mailHandlers) asynq.HandlerContributor { return handlers },
//				),
//			),
//		},
//	)
//
//	func Definition() plugin.Definition { return definition }
//
//	func Bundle() plugin.Bundle { return plugin.BundleOf(Definition()) }
//
//	func newApp() (*xbc.App, error) {
//		return xbc.New(xbc.WithBundles(
//			asynq.Bundle(),
//			Bundle(),
//		))
//	}
//
// Asynq collects contributors as a typed input and freezes their handler
// registrations during construction. A separate producer Plugin can consume
// Enqueuer through a declared plugin.RefTo[asynq.Enqueuer](asynq.Key) input.
// Keeping enqueueing and handler contribution in separate Definitions avoids a
// dependency cycle.
//
// Queue names passed to Queue must exist in plugins.asynq. The runtime owns its
// Redis connection, enqueue client, and worker server. Start submits the worker
// as a critical managed task, which waits for XBC's global traffic gate before
// polling Redis. Stop rejects new enqueue calls, drains the worker, and closes
// Redis. Tasks may be redelivered, so handlers should be idempotent and payloads
// should not contain unprotected secrets.
package asynq
