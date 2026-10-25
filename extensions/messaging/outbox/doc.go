// Package outbox implements a transactional GORM outbox with portable,
// lease-based dispatch. Applications enqueue through the same *gorm.DB
// transaction as their domain write, then replicas safely compete to publish.
//
// # Usage
//
// Definition returns one canonical declaration whose sole primary value is
// *Service. The same value is exported under the Dispatcher contract, for
// transactional enqueue, and under the Manager contract, for retention and
// dead-row replay. Bundle
// is side-effect free and can be composed explicitly with a GORM producer and
// application Definitions:
//
//	app, err := xbc.New(xbc.WithBundles(
//		gormplugin.Bundle(),
//		outbox.Bundle(),
//		orders.Bundle(),
//	))
//
// The configured db_instance selects an exact *gorm.DB producer identity:
//
//	plugins:
//	  outbox:
//	    db_instance: writer
//	    table: xbc_outbox_events
//	    migrate: true
//	    worker:
//	      enabled: true
//
// A consumer declares typed inputs and reads those same tokens in its factory:
//
//	var (
//		ordersDB    = plugin.RefToInstance[*gormlib.DB](gormplugin.Key, "writer")
//		orderEvents = plugin.RequireOne[outbox.Dispatcher]()
//	)
//
//	var ordersDefinition = plugin.Define(
//		"orders",
//		func(ctx plugin.BuildContext) (*orderWriter, error) {
//			return &orderWriter{
//				db:     ordersDB.Get(ctx).Value,
//				events: orderEvents.Get(ctx).Value,
//			}, nil
//		},
//		plugin.Options[*orderWriter]{
//			Inputs: plugin.Inputs(ordersDB, orderEvents),
//		},
//	)
//
//	type orderWriter struct {
//		db     *gormlib.DB
//		events outbox.Dispatcher
//	}
//
//	func (writer *orderWriter) Create(ctx context.Context, order *Order, payload []byte) error {
//		return writer.db.WithContext(ctx).Transaction(func(tx *gormlib.DB) error {
//			if err := tx.Create(order).Error; err != nil {
//				return err
//			}
//			_, err := writer.events.Enqueue(ctx, tx, outbox.Event{
//				Topic:   "orders.created",
//				Key:     order.ID,
//				Payload: payload,
//			})
//			return err
//		})
//	}
//
// Enqueue requires a caller-owned GORM transaction and never starts or commits
// it, preventing the business row and event from being committed separately.
// Run XBC's explicit migration stage with migrate enabled to create the table.
//
// When worker.enabled is true, compose zero or one Plugin exporting Publisher;
// construction fails clearly when none is present. Polling starts only after
// all traffic preparation succeeds and XBC releases the global traffic gate.
// Replicas use renewable leases, but delivery remains at least once, so
// publishers and consumers must use Event.ID for idempotency.
//
// # Retention and replay
//
// No state ever leaves the table on its own unless retention removes it.
// Finished rows are measured from the moment they entered their state:
//
//	plugins:
//	  outbox:
//	    retention:
//	      published: 168h # 0 keeps published rows forever
//	      dead: 0         # 0 keeps dead rows forever (the default)
//	      interval: 1h    # how often the worker runs a cleanup pass
//	      batch_size: 1000
//
// published defaults to 168h, so upgrading deletes published rows older than a
// week; set it to 0 to keep them. dead defaults to 0 because dead rows are what
// an operator investigates and replays. Negative durations are rejected, and
// interval and batch_size must be positive.
//
// When worker.enabled is true and at least one retention is non-zero, the
// worker runs one maintenance goroutine, separate from the claim and publish
// loop, so a slow DELETE never delays delivery. Every interval (the first pass
// after a short random delay, so replicas started together do not align) it
// deletes expired rows in batches of batch_size, each batch under
// worker.database_timeout, and leaves a remaining backlog to the next pass.
// Deletes re-check the state and age of every row they remove, so replicas
// purging at once, or a row replayed while a pass is running, are harmless. A
// replica without worker.enabled never purges on its own; a host can call
// Manager.Purge, for example from a scheduled job, which runs the same bounded
// pass whether or not a worker exists.
//
// Manager.ListDead pages dead rows by id (at most 1000 per page, each with
// LastError), and Manager.Requeue returns the named dead rows (1 to 1000 ids,
// duplicates counted once) to pending with a fresh attempt budget. Requeue only
// acts on rows that are dead at that moment. Replayed rows are delivered again
// at least once, so the usual Event.ID idempotency applies.
//
// outbox opens no HTTP endpoint or command. A host that exposes ListDead or
// Requeue must add its own authentication and authorization: dead rows carry
// the original payload and headers, which may be sensitive.
//
// The first Migrate after upgrading creates the index idx_outbox_retention on
// (status, updated_at). On a large table that is a one-off index build, so run
// the migration stage deliberately rather than at peak traffic.
//
// # Shutdown
//
// Service implements Stop only, not plugin.Drainer, and that is deliberate
// rather than an omission: Service's Options[*Service].Lifecycle supplies
// OpenTraffic, which makes it a real ingress instance -- it opens traffic to
// release the worker's wait on the global traffic gate -- so Service is a
// member of Unwind's ingress closure and is stopped in phase A, before any
// Drain phase (phase B) runs at all. A Drain hook on an instance inside the
// ingress closure is never invoked; phase A already stops it. Any dependent
// that enqueues from its own Stop is also in the ingress closure, because
// that closure is the transitive-dependents set over every TrafficOpener,
// and such a dependent is stopped before Service in the same reverse start
// order, giving it its chance to enqueue before Service closes admission.
//
// Stop itself still performs the two actions a Drain phase would: it closes
// Enqueue and claim admission immediately, then waits for every event
// accepted before that point and for the worker's active publish cycle to
// finish, all detached from the caller's own deadline (see stopAdmission and
// worker.requestStop) so one caller's timeout cannot cut the shared wait
// short for a later caller. The database connection and configured Publisher
// are released only once that wait completes. A caller whose own context
// expires first gets back ctx.Err() without blocking further, but the
// detached wait keeps running underneath and the plugin graph's shared
// shutdown budget still bounds it.
//
// The retention maintenance goroutine belongs to the same worker completion
// signal. Stop cancels it, abandoning a DELETE that is in flight, and waits for
// it to exit before the database is released, whether or not it ever ran; the
// abandoned remainder is picked up by a later pass or another replica. Manager
// calls follow the same admission rule as Enqueue: once Stop begins, new calls
// are rejected with ErrClosed and calls already running are canceled and also
// return ErrClosed.
//
// Event payloads and headers are persisted in the configured database. Protect
// that database appropriately and avoid storing secrets or sensitive data that
// the application's retention and encryption policy does not cover.
package outbox
