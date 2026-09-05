// Package outbox implements a transactional GORM outbox with portable,
// lease-based dispatch. Applications enqueue through the same *gorm.DB
// transaction as their domain write, then replicas safely compete to publish.
//
// # Usage
//
// Definition returns one canonical declaration whose sole primary value is
// *Service. The same value is exported under the Dispatcher contract. Bundle
// is side-effect free and can be composed explicitly with a GORM producer and
// application Definitions:
//
//	app, err := xbc.New(xbc.WithBundles(
//		gormplugin.Bundle(),
//		outbox.Bundle(),
//		orders.Bundle(),
//	))
//
// The configured db_instance selects an exact *gorm.DB producer identity. A
// consumer declares typed inputs and reads those same tokens in its factory:
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
// publishers and consumers must use Event.ID for idempotency. Shutdown stops
// admission and waits for accepted inserts and worker activity to drain.
//
// Event payloads and headers are persisted in the configured database. Protect
// that database appropriately and avoid storing secrets or sensitive data that
// the application's retention and encryption policy does not cover.
package outbox
