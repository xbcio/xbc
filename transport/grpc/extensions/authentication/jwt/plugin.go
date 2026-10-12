package jwt

import (
	"errors"
	"strings"

	jwtcore "github.com/xbcio/xbc/extensions/authentication/jwt"
	"github.com/xbcio/xbc/plugin"
	grpctransport "github.com/xbcio/xbc/transport/grpc"
)

// Key is the stable Definition identity of the gRPC credential extractor. It
// is distinct from the neutral authenticator's own key, jwt, which owns the
// plugins.jwt section: the extractor has no configuration of its own and reads
// its entry and scheme from that authenticator.
const Key plugin.Key = "jwt-grpc"

// Extractor pulls a bearer token out of a call's incoming metadata for the
// neutral JWT authenticator. It decides only whether the configured entry
// syntactically belongs to the jwt scheme; every semantic check stays with the
// authenticator.
type Extractor struct {
	key    string
	scheme string
}

var _ grpctransport.CredentialExtractor = (*Extractor)(nil)

var authenticatorInput = plugin.RefTo[*jwtcore.Plugin](jwtcore.Key)

var definition = plugin.Define(
	Key,
	func(ctx plugin.BuildContext) (*Extractor, error) {
		return New(authenticatorInput.Get(ctx).Value)
	},
	plugin.Options[*Extractor]{
		// The extractor follows the authenticator it pairs with: configuring
		// plugins.jwt turns both on, and leaving it out leaves both dormant.
		Activation: plugin.WhenConfigured("plugins." + jwtcore.Key.String()),
		Inputs:     plugin.Inputs(authenticatorInput),
		Exports: plugin.Contracts(
			plugin.ExportAs[grpctransport.CredentialExtractor](func(value *Extractor) grpctransport.CredentialExtractor { return value }),
		),
	},
)

// bundle selects the neutral authenticator alongside the extractor, so a
// composition root never names both halves.
var bundle = plugin.CombineBundles(jwtcore.Bundle(), plugin.BundleOf(definition))

// New builds an extractor that reads the entry and scheme configured on
// authenticator. gRPC delivers metadata keys in lower case, so the configured
// header name is lower-cased here, once.
func New(authenticator *jwtcore.Plugin) (*Extractor, error) {
	if authenticator == nil {
		return nil, errors.New("jwt-grpc: requires the jwt authenticator")
	}
	return &Extractor{key: strings.ToLower(authenticator.Header()), scheme: authenticator.TokenScheme()}, nil
}

// Definition returns the gRPC extractor's canonical immutable declaration
// handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the side-effect-free explicit composition of the gRPC
// extractor and the neutral JWT authenticator it feeds.
func Bundle() plugin.Bundle { return bundle }
