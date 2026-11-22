// Package casbinx is XBC's protocol-neutral Casbin policy engine: the model and
// policy sources, optional adapter and watcher providers, managed periodic
// reload, lifecycle, and the rbac.Backend and EnforcerProvider exports.
//
// The engine is selectable without any transport. rbac.Backend is the
// authorization contract application code and transport-neutral managers use;
// EnforcerProvider exposes the live *casbin.SyncedEnforcer for advanced
// migration and diagnostic code together with the request convention that
// enforcer was built and validated for. Web route enforcement is a separate
// plugin with its own key and configuration section,
// transport/web/extensions/authorization/casbinx, which consumes this engine's
// EnforcerProvider and adds the route middleware and its authentication
// ordering. RequestConvention configured here selects the model this engine
// builds, every model is validated against it, and a consumer learns the
// resulting shape from EnforcerProvider.RequestConvention -- so a configured
// convention and the enforcer it runs against cannot disagree, and no consumer
// has to read the model LoadPolicy replaces underneath it.
//
// # Usage
//
// Compose the engine and address it through rbac.Manager; grants are policy
// rows, not code:
//
//	func main() {
//		xbc.Run(xbc.WithBundles(casbincore.Bundle(), rbac.Bundle()))
//	}
//
//	plugins:
//	  casbin:
//	    policy: |
//	      p, alice, reports:read
//
// # Sources, adapters, and watchers
//
// Model and ModelFile are mutually exclusive; when neither is set, a secure
// built-in model matching request_convention is used. Inline and file sources
// are loaded during construction and stay read-only (autosave disabled):
// explicit LoadPolicy atomically restores the configured source. An external
// adapter selected through a configured provider is attached without loading
// during construction so XBC's Migrate stage can run first; Start performs the
// initial load, and adapter mode enables Casbin autosave. Adapter is mutually
// exclusive with Policy and PolicyFile -- the adapter is the policy source --
// and Watcher requires Adapter:
//
//	plugins:
//	  casbin-gorm:
//	    default: {}
//	  casbin-redis:
//	    default: {}
//	  casbin:
//	    request_convention: path_method
//	    adapter:
//	      plugin: casbin-gorm
//	      instance: default
//	    watcher:
//	      plugin: casbin-redis
//	      instance: default
//
// Provider references are resolved through Definition/Bundle composition, not
// casbin.New. The adapter provider owns schema migration and its backing
// connection; the watcher factory creates a watcher that this plugin owns
// until shutdown. The engine never parses credentials: identity is the
// caller's business, and an identity a Web adapter derives is that adapter's
// subject resolver.
//
// # Direct construction with an external adapter
//
// A caller that already owns a persist.Adapter -- for example a host keeping
// its own rule table model -- passes it to New with WithAdapter instead of
// naming a provider:
//
//	engine, err := casbinx.New(cfg,
//		casbinx.WithAdapter(adapter),
//		casbinx.WithRoleMatchingFunc("g", "KeyMatch2", matchKeyMatch2),
//	)
//
// The timing differs from the provider path on purpose. Direct construction
// has no XBC Migrate stage and no mandatory Start, so the injected adapter is
// attached and loaded before New returns and Enforce answers from it
// immediately. The configured provider path is unchanged: its adapter is
// attached during construction and first loaded in Start, after migrations,
// which is what New cannot reproduce for a provider it cannot resolve.
//
// Autosave is enabled for an injected adapter, matching the provider path;
// WithAutoSave(false) keeps Casbin mutations in memory until the next
// LoadPolicy. WithAdapter is mutually exclusive with Policy, PolicyFile, and a
// configured Adapter provider reference, because the adapter is the policy
// source. The plugin never closes or migrates an adapter it did not create.
// A configured Watcher still requires a provider resolved by composition.
//
// # Role matching functions
//
// WithRoleMatchingFunc registers a Casbin role matching function for a role
// definition with Casbin's Enforcer.AddNamedMatchingFunc semantics. It is
// applied after the model is built but before the first policy load, so the
// role links that load builds already consult it and later LoadPolicy calls
// keep it. This is the hook for wildcard role subjects: registering KeyMatch2
// lets a grouping rule naming reader:1 satisfy a policy written for reader:*.
// Registration is refused when the model defines no such role definition, so
// a mistyped ptype cannot silently disable matching. Options are available
// only to New; Definition/Bundle composition configures through Config and
// keeps the exact-match default.
//
// Importing this package has no registration side effects. Import
// github.com/xbcio/xbc/extensions/authorization/casbinx/autoload for
// process-wide composition, or use Definition/Bundle with a private assembly.
package casbinx
