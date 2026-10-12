package apikey

import (
	"errors"

	apikeycore "github.com/xbcio/xbc/extensions/authentication/apikey"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is the stable Definition identity of the Web credential extractor. It is
// distinct from the neutral authenticator's own key, apikey, which owns the
// plugins.apikey section: the extractor has no configuration of its own and
// reads its headers and scheme from that authenticator.
const Key plugin.Key = "apikey-http"

// Extractor pulls an API key out of an HTTP request for the neutral API-key
// authenticator. It decides only whether the request syntactically carries
// apikey evidence; every semantic check stays with the authenticator.
type Extractor struct {
	header       string
	appIDHeader  string
	allowBearer  bool
	bearerScheme string
}

var _ web.CredentialExtractor = (*Extractor)(nil)

var authenticatorInput = plugin.RefTo[*apikeycore.Plugin](apikeycore.Key)

var definition = plugin.Define(
	Key,
	func(ctx plugin.BuildContext) (*Extractor, error) {
		return New(authenticatorInput.Get(ctx).Value)
	},
	plugin.Options[*Extractor]{
		// The extractor follows the authenticator it pairs with: configuring
		// plugins.apikey turns both on, and leaving it out leaves both dormant.
		Activation: plugin.WhenConfigured("plugins." + apikeycore.Key.String()),
		Inputs:     plugin.Inputs(authenticatorInput),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.CredentialExtractor](func(value *Extractor) web.CredentialExtractor { return value }),
		),
	},
)

// bundle selects the neutral authenticator alongside the extractor. An
// extractor without the authenticator it feeds is not a composition anyone
// wants, and RefTo would reject it at planning time anyway.
var bundle = plugin.CombineBundles(apikeycore.Bundle(), plugin.BundleOf(definition))

// New builds an extractor that reads the headers and scheme configured on
// authenticator.
func New(authenticator *apikeycore.Plugin) (*Extractor, error) {
	if authenticator == nil {
		return nil, errors.New("apikey-http: requires the apikey authenticator")
	}
	return &Extractor{
		header:       authenticator.Header(),
		appIDHeader:  authenticator.AppIDHeader(),
		allowBearer:  authenticator.AllowBearer(),
		bearerScheme: authenticator.BearerScheme(),
	}, nil
}

// Definition returns the Web extractor's canonical immutable declaration
// handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the side-effect-free explicit composition of the Web
// extractor and the neutral API-key authenticator it feeds.
func Bundle() plugin.Bundle { return bundle }
