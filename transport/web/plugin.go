package web

import (
	"github.com/xbcio/xbc/authentication"
	"github.com/xbcio/xbc/plugin"
)

const (
	// Key is the stable identity of the HTTP server Plugin.
	Key plugin.Key = "web"

	// ErrorBoundaryKey identifies the independently ordered error boundary.
	ErrorBoundaryKey plugin.Key = "web-error-boundary"

	// AuthenticationMiddlewareKey is the canonical producer identity expected
	// for the Web authentication middleware. RequiresPrincipal entries are
	// framework-pinned after this key.
	AuthenticationMiddlewareKey plugin.Key = "authentication-middleware"

	// ConfigPath is the canonical configuration section for the HTTP server.
	ConfigPath = "web"
)

var (
	middlewareInput = plugin.Collect[Middleware]()
	routeInput      = plugin.Collect[RouteContributor]()
	listenerInput   = plugin.Collect[RouteCatalogListener]()
	// The Server, not a separate Definition, assembles the built-in
	// authentication middleware: the policy it enforces lives in this
	// Definition's own web.security section, and a configuration section has
	// exactly one owning plugin.
	authenticatorInput = plugin.Collect[authentication.Authenticator]()
	extractorInput     = plugin.Collect[CredentialExtractor]()

	definition = plugin.DefineConfigured(
		Key,
		plugin.ConfigSpec[Config]{
			Defaults: DefaultConfig,
			Prepare:  normalizeConfig,
		},
		func(ctx plugin.BuildContext, cfg Config) (*Server, error) {
			return newServer(
				cfg,
				middlewareInput.Get(ctx),
				routeInput.Get(ctx),
				listenerInput.Get(ctx),
				authenticatorInput.Get(ctx),
				extractorInput.Get(ctx),
			), nil
		},
		plugin.Options[*Server]{
			ConfigPath: ConfigPath,
			Inputs: plugin.Inputs(
				middlewareInput,
				routeInput,
				listenerInput,
				authenticatorInput,
				extractorInput,
			),
		},
	)

	bundle = plugin.BundleOf(definition, errorBoundaryDefinition)
)

// New constructs a side-effect-free server with production-safe defaults.
// Contributions are normally injected by Definition; tests and embedding hosts
// can use the unexported constructor in this package.
func New() *Server { return newServer(DefaultConfig(), nil, nil, nil, nil, nil) }

// Definition returns the canonical HTTP server declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns Web's server and independently ordered error boundary.
func Bundle() plugin.Bundle { return bundle }
