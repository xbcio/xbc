package grpc

import (
	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/plugin"
)

const (
	// Key is the stable identity of the gRPC server Plugin.
	Key plugin.Key = "grpc"

	// ConfigPath is the canonical configuration section for the gRPC server.
	ConfigPath = "grpc"
)

var (
	registrarInput = plugin.Collect[ServiceRegistrar]()
	unaryInput     = plugin.Collect[UnaryInterceptor]()
	streamInput    = plugin.Collect[StreamInterceptor]()
	authInput      = plugin.Collect[authentication.Authenticator]()
	extractorInput = plugin.Collect[CredentialExtractor]()
	mapperInput    = plugin.Collect[StatusMapper]()

	definition = plugin.DefineConfigured(
		Key,
		plugin.ConfigSpec[Config]{
			Defaults: DefaultConfig,
			Prepare:  normalizeConfig,
		},
		func(ctx plugin.BuildContext, cfg Config) (*Server, error) {
			return newServer(cfg, contributions{
				registrars:     registrarInput.Get(ctx),
				unary:          unaryInput.Get(ctx),
				stream:         streamInput.Get(ctx),
				authenticators: authInput.Get(ctx),
				extractors:     extractorInput.Get(ctx),
				mappers:        mapperInput.Get(ctx),
			}), nil
		},
		plugin.Options[*Server]{
			ConfigPath: ConfigPath,
			Inputs:     plugin.Inputs(registrarInput, unaryInput, streamInput, authInput, extractorInput, mapperInput),
		},
	)

	bundle = plugin.BundleOf(definition)
)

// New constructs a side-effect-free server with production-safe defaults and no
// contributions. Contributions are normally injected by Definition.
func New() *Server {
	return newServer(DefaultConfig(), contributions{})
}

// Definition returns the canonical gRPC server declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the gRPC server. The panic boundary, status boundary, and
// authentication stage travel inside it rather than as separate Definitions,
// because the Server assembles them; see ReservedInterceptorKeys.
func Bundle() plugin.Bundle { return bundle }
