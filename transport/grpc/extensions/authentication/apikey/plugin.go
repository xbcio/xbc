package apikey

import (
	"errors"
	"strings"

	apikeycore "github.com/xbcio/xbc/extensions/authentication/apikey"
	"github.com/xbcio/xbc/plugin"
	grpctransport "github.com/xbcio/xbc/transport/grpc"
)

// Key is the stable Definition identity of the gRPC credential extractor. It
// is distinct from the neutral authenticator's own key, apikey, which owns the
// plugins.apikey section: the extractor has no configuration of its own and
// reads its keys and scheme from that authenticator.
const Key plugin.Key = "apikey-grpc"

// Extractor pulls an API key out of a call's incoming metadata for the neutral
// API-key authenticator. It decides only whether the call syntactically
// carries apikey evidence; every semantic check stays with the authenticator.
type Extractor struct {
	key          string
	appIDKey     string
	allowBearer  bool
	bearerScheme string
}

var _ grpctransport.CredentialExtractor = (*Extractor)(nil)

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
			plugin.ExportAs[grpctransport.CredentialExtractor](func(value *Extractor) grpctransport.CredentialExtractor { return value }),
		),
	},
)

// bundle selects the neutral authenticator alongside the extractor, so a
// composition root never names both halves.
var bundle = plugin.CombineBundles(apikeycore.Bundle(), plugin.BundleOf(definition))

// New builds an extractor that reads the entries and scheme configured on
// authenticator. gRPC delivers metadata keys in lower case, so the configured
// header names are lower-cased here, once.
func New(authenticator *apikeycore.Plugin) (*Extractor, error) {
	if authenticator == nil {
		return nil, errors.New("apikey-grpc: requires the apikey authenticator")
	}
	return &Extractor{
		key:          strings.ToLower(authenticator.Header()),
		appIDKey:     strings.ToLower(authenticator.AppIDHeader()),
		allowBearer:  authenticator.AllowBearer(),
		bearerScheme: authenticator.BearerScheme(),
	}, nil
}

// Definition returns the gRPC extractor's canonical immutable declaration
// handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the side-effect-free explicit composition of the gRPC
// extractor and the neutral API-key authenticator it feeds.
func Bundle() plugin.Bundle { return bundle }
