package jwt

import (
	"errors"

	jwtcore "github.com/xbcio/xbc/extensions/authentication/jwt"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is the stable Definition identity of the Web credential extractor. It is
// distinct from the neutral authenticator's own key, jwt, which owns the
// plugins.jwt section: the extractor has no configuration of its own and reads
// its header and scheme from that authenticator.
const Key plugin.Key = "jwt-http"

// Extractor pulls a bearer token out of an HTTP request for the neutral JWT
// authenticator. It decides only whether the configured header syntactically
// belongs to the jwt scheme; every semantic check stays with the authenticator.
type Extractor struct {
	header string
	scheme string
}

var _ web.CredentialExtractor = (*Extractor)(nil)

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
			plugin.ExportAs[web.CredentialExtractor](func(value *Extractor) web.CredentialExtractor { return value }),
		),
	},
)

// bundle selects the neutral authenticator alongside the extractor. An
// extractor without the authenticator it feeds is not a composition anyone
// wants, and RefTo would reject it at planning time anyway.
var bundle = plugin.CombineBundles(jwtcore.Bundle(), plugin.BundleOf(definition))

// New builds an extractor that reads the header and scheme configured on
// authenticator.
func New(authenticator *jwtcore.Plugin) (*Extractor, error) {
	if authenticator == nil {
		return nil, errors.New("jwt-http: requires the jwt authenticator")
	}
	return &Extractor{header: authenticator.Header(), scheme: authenticator.TokenScheme()}, nil
}

// Definition returns the Web extractor's canonical immutable declaration
// handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the side-effect-free explicit composition of the Web
// extractor and the neutral JWT authenticator it feeds.
func Bundle() plugin.Bundle { return bundle }
