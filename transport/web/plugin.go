package web

import (
	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/plugin"
)

const (
	// Key is the stable identity of the HTTP server Plugin.
	Key plugin.Key = "web"

	// ErrorBoundaryKey is the canonical producer identity of the outermost
	// error boundary. The Server assembles that boundary itself from the
	// collected ErrorMapper plugins, so this key is reserved and no Definition
	// may claim it; see ReservedMiddlewareKeys. Ordering against it inside
	// PhaseError is the supported use of the key.
	ErrorBoundaryKey plugin.Key = "web-error-boundary"

	// AuthenticationMiddlewareKey is the canonical producer identity expected
	// for the Web authentication middleware. RequiresPrincipal entries are
	// framework-pinned after this key. The Server assembles that middleware
	// itself, so this key is reserved and no Definition may claim it; see
	// ReservedMiddlewareKeys.
	AuthenticationMiddlewareKey plugin.Key = "authentication-middleware"

	// ConfigPath is the canonical configuration section for the HTTP server.
	ConfigPath = "web"
)

var (
	middlewareInput = plugin.Collect[Middleware]()
	routeInput      = plugin.Collect[RouteContributor]()
	listenerInput   = plugin.Collect[RouteCatalogListener]()
	// The Server, not a separate Definition, assembles the outermost error
	// boundary and the built-in authentication middleware. Both are stages a
	// framework-owned ordering pin makes required, and a required stage cannot
	// be an opt-in plugin: omitting or disabling it would either serve every
	// route unauthenticated or leave every contributed ErrorMapper unreachable.
	// Enforcing web.security is additionally this Definition's own config
	// section, which has exactly one owning plugin. Both keys are reserved
	// accordingly; see reserved_middleware.go.
	errorMapperInput   = plugin.Collect[ErrorMapper]()
	authenticatorInput = plugin.Collect[authentication.Authenticator]()
	extractorInput     = plugin.Collect[CredentialExtractor]()
	// engineInput selects the HTTP engine adapter. web.Server never
	// constructs a concrete engine itself: every adapter package imports web
	// to implement Engine, so if web imported an adapter back, that engine's
	// dependencies (gin, or whatever the adapter wraps) would be pulled back
	// into web's own dependency closure -- exactly what keeping engines as
	// separate modules is meant to prevent. A composition root selects
	// exactly one engine Bundle (transport/web/engines/gin, or another
	// Engine adapter) alongside web.Bundle() to satisfy this input.
	engineInput = plugin.RequireOne[EngineFactory]()

	definition = plugin.DefineConfigured(
		Key,
		plugin.ConfigSpec[Config]{
			Defaults: DefaultConfig,
			Prepare:  normalizeConfig,
		},
		func(ctx plugin.BuildContext, cfg Config) (*Server, error) {
			return newServer(
				cfg,
				engineInput.Get(ctx).Value,
				middlewareInput.Get(ctx),
				errorMapperInput.Get(ctx),
				routeInput.Get(ctx),
				listenerInput.Get(ctx),
				authenticatorInput.Get(ctx),
				extractorInput.Get(ctx),
			), nil
		},
		plugin.Options[*Server]{
			ConfigPath: ConfigPath,
			Inputs: plugin.Inputs(
				engineInput,
				middlewareInput,
				errorMapperInput,
				routeInput,
				listenerInput,
				authenticatorInput,
				extractorInput,
			),
		},
	)

	bundle = plugin.BundleOf(definition)
)

// New constructs a side-effect-free server with production-safe defaults.
// Contributions are normally injected by Definition; tests and embedding
// hosts supply their own EngineFactory (an engine adapter's Factory, or a
// test double) since Start fails without one.
func New(factory EngineFactory) *Server {
	return newServer(DefaultConfig(), factory, nil, nil, nil, nil, nil, nil)
}

// Definition returns the canonical HTTP server declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns Web's HTTP server. The error boundary and the authentication
// middleware travel inside it rather than as separate Definitions, because the
// Server assembles both; see ReservedMiddlewareKeys.
func Bundle() plugin.Bundle { return bundle }
