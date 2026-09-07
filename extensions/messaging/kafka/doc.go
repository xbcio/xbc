// Package kafka provides XBC-managed Kafka producers and consumers behind
// protocol-neutral Message, Producer, and Handler contracts. Importing this
// package is side-effect free. Applications compose Bundle explicitly;
// executables may instead import the autoload subpackage when they intentionally
// want process-wide default composition.
//
// # Usage
//
// Each named section below plugins.kafka constructs one concrete *kafka.Client
// and exports kafka.Producer. A definition that owns consumer behavior declares
// an exact typed input and registers its handler during construction, before
// lifecycle Start:
//
//	var cluster = plugin.RefToInstance[*kafka.Client](kafka.Key, "events")
//
//	type ordersConsumer struct{}
//
//	func (*ordersConsumer) handle(context.Context, kafka.Message) error { return nil }
//
//	var ordersDefinition = plugin.Define(
//		"orders-consumer",
//		func(ctx plugin.BuildContext) (*ordersConsumer, error) {
//			consumer := &ordersConsumer{}
//			if err := cluster.Get(ctx).Value.RegisterHandler(
//				"orders",
//				kafka.HandlerFunc(consumer.handle),
//			); err != nil {
//				return nil, err
//			}
//			return consumer, nil
//		},
//		plugin.Options[*ordersConsumer]{
//			Inputs: plugin.Inputs(cluster),
//		},
//	)
//
//	func Definition() plugin.Definition { return ordersDefinition }
//
//	func Bundle() plugin.Bundle { return plugin.BundleOf(ordersDefinition) }
//
//	func applicationBundle() plugin.Bundle {
//		return plugin.CombineBundles(kafka.Bundle(), Bundle())
//	}
//
// Start freezes registrations, constructs configured readers, and submits each
// consumer as a critical managed task. Those tasks wait for XBC's global traffic
// gate before fetching. A consumer commits only after its handler succeeds (or
// after the configured skip policy). Commits are synchronous so broker errors
// stay observable. Stop rejects production, cancels and joins consumers, then
// closes readers and the producer.
//
// TLS supports private CAs and client certificates. SASL PLAIN or SCRAM is
// accepted only with TLS. Keep credentials in secret-backed configuration and
// make handlers idempotent because broker redelivery is possible.
package kafka
