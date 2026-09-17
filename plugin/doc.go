// Package plugin defines xbc's protocol-neutral plugin SPI. A Definition is
// the immutable declaration of one runtime unit: it owns a stable Key and,
// where applicable, its configuration, typed Inputs, contracts, factory, and
// lifecycle capabilities. A Bundle is an explicit, side-effect-free static
// composition of zero or more canonical Definitions. XBC has no process-global
// catalog: every dependency and contract is declared statically on a Definition
// and resolved by its assembly stage before any factory runs, never looked up
// live.
//
// Plugin names that runtime unit; it is not a marker interface, a base type, a
// Go package, or a Bundle. A concrete primary value P belongs to a Plugin only
// because an immutable Definition declares and owns it; an ordinary third-party
// type can be a primary value directly, and a type that cannot implement XBC's
// lifecycle interfaces itself is adapted through Options[P].Lifecycle (see
// below) instead of being wrapped or subclassed.
//
// # Identity
//
// Key is the stable, configuration-facing name of a Definition, chosen by
// its author and never derived from a Go package path, type name, or
// variable name. Cardinality decides how many instances one enabled
// Definition produces: SingleInstance, the default, always yields exactly
// plugin.DefaultInstance; MultipleInstances expands the configuration
// section's direct child keys into named instances instead. A Key plus a
// normalized instance name form Identity, the one handle used everywhere:
// logs, the dependency graph, contract resolution, and start/shutdown
// reporting.
//
//	plugin.Identity{Plugin: "redis", Instance: "cache"}.String() // "redis[cache]"
//	plugin.Identity{Plugin: "redis"}.String()                    // "redis"
//
// CompareIdentity orders identities canonically by (Key, normalized
// instance); that order is what makes graph order and Many[T]'s collected
// slice deterministic across runs.
//
// # Declaring a Definition
//
// Define, DefineConfigured, and DefinePlanned each build one canonical,
// immutable Definition. A package may declare zero or more Definitions, but
// must evaluate every Definition it owns once at package scope. When a package
// exposes Definition(), that accessor conventionally returns its primary
// Definition; it does not say that Bundle() contains only that Definition.
// Calling Define* again inside an accessor would mint a second handle for the
// same Key, which XBC's Plan rejects as a source-aware collision.
//
// Define is for a factory that needs no configuration and whose Inputs are
// fixed:
//
//	var definition = plugin.Define(
//		Key,
//		func(plugin.BuildContext) (*Plugin, error) { return &Plugin{}, nil },
//		plugin.Options[*Plugin]{
//			Exports: plugin.Contracts(
//				plugin.ExportAs[web.RouteContributor](func(value *Plugin) web.RouteContributor { return value }),
//			),
//		},
//	)
//
//	func Definition() plugin.Definition { return definition }
//
// DefineConfigured adds a ConfigSpec[C]: Defaults must return a fresh value
// on every call, Prepare performs only pure semantic normalization and
// validation, and the factory receives the prepared config alongside
// BuildContext. Inputs stay static; configuration decides values, not
// which dependencies exist:
//
//	var definition = plugin.DefineConfigured(
//		Key,
//		plugin.ConfigSpec[Config]{Defaults: DefaultConfig, Prepare: prepareConfig},
//		func(_ plugin.BuildContext, cfg Config) (*Plugin, error) {
//			return New(cfg)
//		},
//		plugin.Options[*Plugin]{
//			Activation: plugin.WhenConfigured("plugins." + Key.String()),
//		},
//	)
//
// DefinePlanned is for the rarer case where the prepared configuration
// itself decides which Inputs exist, not just their values, such as
// selecting one of several optional producers. Its planner runs once per
// enabled instance before graph wiring and must stay pure: no I/O, no
// dependency reads, no lifecycle context.
//
//	func plan(config Config) (plugin.Plan[*Service], error) {
//		database := plugin.RefToInstance[*gorm.DB](gormKey, config.DBInstance)
//		return plugin.PlanOf(
//			plugin.Inputs(database),
//			func(ctx plugin.BuildContext) (*Service, error) {
//				return newService(config, database.Get(ctx).Value), nil
//			},
//		), nil
//	}
//
//	var definition = plugin.DefinePlanned(Key, spec, plan, plugin.Options[*Service]{})
//
// Prefer Define or DefineConfigured whenever the dependency set is fixed;
// reach for DefinePlanned only when it genuinely is not.
//
// # Options
//
// Options[P] carries the static metadata shared by every instance of one
// Definition:
//
//	plugin.Options[*Plugin]{
//		Instances:  plugin.MultipleInstances,
//		Activation: plugin.WhenConfigured("plugins.fixture"),
//		ConfigPath: "services.fixture",
//	}
//
// Instances selects Cardinality (see Identity above). Activation's zero
// value means the Definition is enabled whenever it is composed, unless
// configuration explicitly sets "enabled: false"; WhenConfigured(path)
// instead requires path to exist in the merged environment. ConfigPath
// overrides the conventional "plugins.<key>" section with an owner-chosen
// canonical path — transport/web, for example, owns the root-level "web"
// section — and it governs that enabled/disabled check even for a
// Definition with no ConfigSpec at all. Exports and Lifecycle are covered
// next.
//
// # Exporting contracts
//
// A Definition's primary concrete type P is automatically a resolvable
// contract. Any additional interface must be exported explicitly with
// ExportAs, whose witness function — func(P) I, not a bare interface
// argument — makes the Go compiler prove P is assignable to I. A primary
// type that does not actually implement I fails at go build, not inside
// XBC's Plan:
//
//	plugin.Options[*Plugin]{
//		Exports: plugin.Contracts(
//			plugin.ExportAs[web.RouteContributor](func(value *Plugin) web.RouteContributor { return value }),
//		),
//	}
//
// Plan still rejects a nil witness, a non-interface contract type, and the
// same contract exported twice. ExportAs proves assignability; it does not
// create an anonymous value, and every resolution keeps the producing
// Identity alongside the exported value (see Entry[T] below).
//
// # Declaring and resolving dependencies
//
// A factory's dependencies are typed Input tokens, not live lookups. Ref[T]
// selects one exact (Key, Instance); One[T] requires exactly one exporter
// of T anywhere in the graph; Optional[T] allows zero or one; Many[T]
// collects every exporter as a deterministic, graph-ordered slice. Their
// constructors — RefTo, RefToInstance, RequireOne, OptionalOne, Collect —
// return immutable, reusable metadata with no per-App binding, so a token
// is safe to declare once at package scope and read concurrently:
//
//	var controllerInput = plugin.RefTo[*Controller](Key)
//
//	var definition = plugin.DefineConfigured(
//		HTTPKey,
//		plugin.ConfigSpec[Config]{Defaults: defaultConfig, Prepare: prepareConfig},
//		func(ctx plugin.BuildContext, cfg Config) (*Plugin, error) {
//			return newPlugin(cfg, controllerInput.Get(ctx).Value)
//		},
//		plugin.Options[*Plugin]{
//			Inputs: plugin.Inputs(controllerInput),
//		},
//	)
//
// The same token value must appear in both Options[P].Inputs (or Plan's
// InputSet) and the factory's Get call; reading a token that was never
// declared there panics with an internal-invariant message instead of
// degrading into a live lookup. Every edge, ambiguity, and missing producer
// is therefore known before Construct calls a single factory. Get returns
// Entry[T]{Identity, Value}; Collect[T]() is the many-producer form, used
// for example as plugin.Collect[RouteContributor]() to gather every route
// contributor a composition happens to include.
//
// # Lifecycle capabilities
//
// A primary value opts into up to five stages by implementing the matching
// interface directly: Initializer, Migrator, Runner, TrafficOpener, and
// Closer. None is required, and P inherits none of them from a base type —
// a bare struct with no methods is a perfectly valid, lifecycle-free
// Plugin.
//
// When P should not implement a stage itself — a third-party type, or a
// method you would rather keep unexported — supply a typed adapter on
// Options[P].Lifecycle instead. An adapter's signature is func(P, *Context)
// error (func(P, context.Context) error for Stop), so an unexported method
// value satisfies it directly without ever implementing the exported
// interface:
//
//	plugin.Lifecycle[*Service]{
//		Migrate:     (*Service).migrate,
//		Start:       (*Service).start,
//		OpenTraffic: (*Service).openTraffic,
//		Stop:        (*Service).stop,
//	}
//
// A single stage can be adapted the same way, such as closing a client XBC
// does not otherwise know how to stop:
//
//	plugin.Lifecycle[*goredis.Client]{Stop: stopClient}
//
//	func stopClient(client *goredis.Client, _ context.Context) error {
//		if err := client.Close(); err != nil && !errors.Is(err, goredis.ErrClosed) {
//			return err
//		}
//		return nil
//	}
//
// The two channels are mutually exclusive per stage: if P already
// implements, say, Runner, and Options[P].Lifecycle.Start is also set,
// XBC's Plan rejects the Definition because it cannot tell which one you
// meant to run.
//
// # Bundles
//
// Bundle is an explicit, side-effect-free static composition of zero or more
// canonical Definitions. It has no Key, configuration, dependencies, or
// lifecycle of its own, and is never itself a Plugin or runtime unit. A
// package's optional Definition() accessor identifies its primary Definition;
// Bundle() instead describes the package's full selectable composition. It may
// contain only that primary Definition, additional independently declared
// Definitions, or no Definitions at all.
//
// A package that owns several runtime units composes them with BundleOf. For
// example, transport/web exposes its HTTP server Definition as its primary
// Definition while its Bundle also selects an independently ordered error
// boundary. gracefulshutdown similarly selects its programmatic Controller and
// its HTTP route contributor as separate Definitions, so each keeps its own
// Key, configuration, contracts, dependencies, and lifecycle behavior:
//
//	var bundle = plugin.BundleOf(controllerDefinition, httpDefinition)
//
//	func Definition() plugin.Definition { return controllerDefinition }
//
//	func Bundle() plugin.Bundle { return bundle }
//
// A pure aggregating package owns no Definition of its own and can expose only
// Bundle. For example, transport/web/prelude flattens several members' Bundles
// with CombineBundles, preserving each entry's original declaration source for
// diagnostics:
//
//	var bundle = plugin.CombineBundles(
//		web.Bundle(),
//		recovery.Bundle(),
//		requestid.Bundle(),
//	)
//
// Both constructors only copy data: importing a package that calls them,
// or calling Bundle() itself, never creates a resource, starts a
// goroutine, or mutates process state. The same canonical Definition
// reached through two different Bundles is deduplicated by declaration
// identity; two different Definitions that happen to share a Key are a
// source-aware collision that Plan rejects, not something a Bundle
// silently resolves.
//
// # BuildContext and Context
//
// XBC hands a factory exactly one BuildContext for the duration of that
// single call. It exposes only Identity(), Log(), and the Get method on
// each declared Input; every copy shares the same synchronized state and
// panics if used after the factory returns, so it must never be retained
// by a background goroutine.
//
// Once a factory succeeds, its lifecycle hooks instead receive a *Context,
// which additionally implements context.Context and adds the
// runtime-scoped capabilities that configuration and dependency wiring
// deliberately exclude from BuildContext:
//
//	func (s *Server) Start(ctx *plugin.Context) error {
//		gate := ctx.TrafficGate()
//		accepted := ctx.GoCritical(func(taskCtx context.Context) {
//			select {
//			case <-gate:
//			case <-taskCtx.Done():
//				return
//			}
//			// start serving only once every Plugin's traffic
//			// preparation has succeeded
//		})
//		if !accepted {
//			return errors.New("serving task rejected outside Start admission")
//		}
//		return nil
//	}
//
// ctx.Go and ctx.GoCritical accept submissions only while that same
// Plugin's Start is executing; both return false once the admission window
// has closed. Only a critical task counts as a long-lived capability: an
// application whose Plugins neither open traffic nor submit one is refused
// at startup rather than left idling until a signal. ctx.TrafficGate()
// stays open (unclosed, so every receive blocks) throughout fallible
// traffic preparation and is closed exactly
// once, by the runtime, only after every participant's OpenTraffic has
// succeeded — never by a Plugin itself. ctx.RequestShutdown(reason) asks
// the whole application to stop and is safe to call from any goroutine a
// Plugin owns, at any time. Ordinary Plugins never construct a Context
// themselves; NewRuntimeContext is framework assembly API.
package plugin
