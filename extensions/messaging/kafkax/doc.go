// Package kafkax provides XBC-managed Kafka producers and consumers behind
// protocol-neutral Message, Producer, and Handler contracts. Importing this
// package is side-effect free. Applications compose Bundle explicitly;
// executables may instead import the autoload subpackage when they intentionally
// want process-wide default composition.
//
// # Usage
//
// Each named section below plugins.kafka constructs one concrete *kafkax.Client
// and exports kafkax.Producer. A definition that owns consumer behavior declares
// an exact typed input and registers its handler during construction, before
// lifecycle Start:
//
//	var cluster = plugin.RefToInstance[*kafkax.Client](kafkax.Key, "events")
//
//	type ordersConsumer struct{}
//
//	func (*ordersConsumer) handle(context.Context, kafkax.Message) error { return nil }
//
//	var ordersDefinition = plugin.Define(
//		"orders-consumer",
//		func(ctx plugin.BuildContext) (*ordersConsumer, error) {
//			consumer := &ordersConsumer{}
//			if err := cluster.Get(ctx).Value.RegisterHandler(
//				"orders",
//				kafkax.HandlerFunc(consumer.handle),
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
//		return plugin.CombineBundles(kafkax.Bundle(), Bundle())
//	}
//
// Start freezes registrations, constructs configured readers, and submits each
// consumer as a critical managed task. Those tasks wait for XBC's global traffic
// gate before fetching. A consumer commits only after its handler succeeds (or
// after the configured skip policy). Commits are synchronous so broker errors
// stay observable. Drain stops each consumer fetching another message and waits
// for the message it holds to be handled and committed, while production stays
// open. Stop rejects production, cancels and joins consumers, then closes
// readers and the producer. One cleanup runs for all callers, in the
// background once it is started, so every Stop caller returns at its own
// context deadline even when a handler has not yet returned.
//
// TLS supports private CAs and client certificates. SASL PLAIN is accepted
// only with TLS because it sends the password itself; SCRAM proves the
// password without sending it and is also accepted on a plaintext listener,
// where the messages that follow remain unencrypted. Keep credentials in
// secret-backed configuration and make handlers idempotent because broker
// redelivery is possible.
//
// # Direct construction
//
// A host that owns kafka-go readers and writers without an XBC application can
// construct the same values directly. NewReader returns a *kafka.Reader and
// NewWriter returns a *kafka.Writer; the caller owns the result and releases
// it with Close, which kafka-go makes idempotent. Options keep the call sites
// short, and a ConnectionOption applies to either constructor:
//
//	reader, err := kafkax.NewReader(kafkax.ReaderConfig{},
//		kafkax.WithBrokers("broker.internal:9092"),
//		kafkax.WithGroup("workers", "orders", "events"),
//		kafkax.WithStartOffset(kafkax.OffsetLatest),
//	)
//	if err != nil {
//		return err
//	}
//	defer func() { _ = reader.Close() }()
//
//	writer, err := kafkax.NewWriter(kafkax.WriterConfig{},
//		kafkax.WithBrokers("broker.internal:9092"),
//		kafkax.WithTopic("events"),
//		kafkax.WithRequiredAcks(kafkax.AckAll),
//	)
//
// ReaderOption and WriterOption are separate types; ConnectionOption -- brokers,
// client id, dial timeout, TLS, SASL, dialer, loggers, and topic -- satisfies
// both, so one value can be passed to either constructor. Options are applied in
// order to a private copy of the configuration, so they override configured
// values, and a nil option is ignored. The struct-literal form remains valid:
// NewReader(cfg) and NewWriter(cfg) behave exactly as before.
//
// NormalizeReader and NormalizeWriter return the configuration NewReader and
// NewWriter would use -- defaults applied and cross-field rules checked --
// without constructing anything, so a caller mapping an existing configuration
// can inspect the effective values first. Neither function modifies its input.
//
// The direct path applies kafka-go's documented defaults rather than the plugin
// binder's. Three reader defaults therefore differ from a plugin consumer
// section: max_bytes is 1,000,000 instead of 10 MiB, partition watching is off
// instead of on, and an empty balance_strategy leaves kafka-go's
// range-then-round-robin offer list instead of restricting it to range. A
// caller that wants the plugin values sets them explicitly. Writer defaults
// already agree with kafka-go's.
//
// The security rules are the same ones the plugin enforces, with one explicit
// opt-out: SASL PLAIN without TLS is rejected unless AllowInsecurePlain is set,
// which exists only to preserve a legacy broker that accepts cleartext
// credentials. The plugin path never enables it. SCRAM is accepted without TLS.
// An unknown mechanism is an error on both paths. A caller-supplied Dialer
// replaces the one built from the connection fields and owns its own transport
// security; configuring both a Dialer and a TLS or SASL block is rejected
// rather than silently preferring one. Logger and ErrorLogger inject kafka-go's
// own logging callbacks; nil keeps kafka-go's silent default.
//
// A direct reader commits manually: CommitInterval is 0, so ReadMessage commits
// synchronously and FetchMessage callers control the commit point through
// CommitMessages. Keep credentials in secret-backed configuration. Do not run
// one broker connection through both a plugin instance and the direct
// constructors in the same process -- two clients are difficult to attribute
// when behavior differs.
//
// # Connection resolution
//
// A deployment whose brokers and credentials come from a control plane sets
// plugins.kafka.<instance>.resolve_connection: true. The instance's configured
// brokers value is then an opaque application-defined reference -- an HTTP
// access-interface URL, say -- and construction passes it, together with the
// configured SASL block, to a kafkax.ConnectionResolver the application exports
// and selects as this package's optional input. The resolver returns the
// effective brokers and SASL once; the returned values replace the configured
// ones and run through the same validation as configuration, while TLS always
// comes from configuration. The single call is bounded by resolve_timeout,
// which defaults to the effective dial_timeout, and a resolver error or a
// rejected result fails construction without rendering credentials. An
// instance that does not enable the flag never consults a resolver, and an
// enabled instance without a selected resolver fails construction. Because
// validate constructs plugins, it really reaches the resolver; doctor only
// prepares configuration and never does.
//
// # Readiness
//
// The primary Client exports health.Contributor and contributes broker
// reachability as a readiness check named after the producing identity, "kafka"
// or "kafka[<instance>]". Nothing else here observes the cluster -- the writer
// batches lazily and consumer loops only log -- so an unreachable or
// unauthenticated cluster is invisible until a Produce call fails. The check asks
// one broker for cluster metadata through the writer's own Dialer, so it
// traverses the configured TLS and SASL path and writes nothing. Reaching a
// single broker is enough: requiring all of them would report the instance as
// unable to serve during an ordinary rolling restart. The contribution is inert
// unless the application also selects the health capability Bundle.
package kafkax
