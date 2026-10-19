package saml

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	federationcore "github.com/xbcio/xbc/extensions/authentication/federation"
	samlcore "github.com/xbcio/xbc/extensions/authentication/saml"
	transportweb "github.com/xbcio/xbc/transport/web"
	federationweb "github.com/xbcio/xbc/transport/web/extensions/authentication/federation"
)

const (
	// stateBytes is the entropy one login transaction's RelayState carries.
	// Sixteen bytes are 128 bits, the minimum the federation contract allows.
	stateBytes = 16
	// maxFormBodyBytes bounds the whole application/x-www-form-urlencoded body
	// before it is parsed. A base64-encoded 1 MiB response is about 1.4 MiB,
	// and percent-encoding its alphabet expands every byte to three, so the
	// bound leaves room for the worst legal encoding while still rejecting an
	// oversized body before it is copied.
	maxFormBodyBytes = 6 << 20
	// maxDecodedResponseBytes mirrors the neutral SAML parser's own document
	// limit. The decoded value is checked here as well so an oversized
	// SAMLResponse never reaches the parser.
	maxDecodedResponseBytes = 1 << 20
	// metadataContentType is the registered media type for SAML metadata.
	metadataContentType = "application/samlmetadata+xml"
)

// RegisterRoutes implements web.RouteContributor.
//
// All three routes are explicitly public. Start and the assertion consumer
// service are the two halves of a login, so the browser reaching them has no
// session yet by definition; the metadata document is published to IdPs and
// administrators who cannot hold a session either. Every authentication
// decision happens after the response has been verified, when the shared
// federation base hands the resolved identity to a new session.
//
// The routes live under the configured prefix and take the provider name as a
// path parameter. A malformed or unregistered provider is answered with the
// same fixed failure code on the login routes and with an empty 404 on
// metadata; the submitted value is never echoed back.
func (p *Plugin) RegisterRoutes(router *transportweb.Router) {
	group := router.Group(p.config.prefix)
	group.GET("/:provider/start", p.start).
		Name("saml.start").
		Auth(transportweb.Public())
	group.POST("/:provider/acs", p.acs).
		Name("saml.acs").
		Auth(transportweb.Public())
	group.GET("/:provider/metadata", p.metadata).
		Name("saml.metadata").
		Auth(transportweb.Public())
}

// start begins one SP-initiated login: the neutral service provider generates
// an AuthnRequest and its redirect URL, the shared base stores the transaction
// with the request ID it must answer, and the browser is sent to the IdP.
func (p *Plugin) start(ctx context.Context, c *transportweb.Ctx) error {
	name, provider, ok := p.lookup(c)
	if !ok {
		p.login.Fail(c, federationweb.CodeBadRequest)
		return nil
	}

	state, err := newState()
	if err != nil {
		p.log().Error("saml login could not generate a relay state", "provider", name, "error", err)
		p.login.Fail(c, federationweb.CodeFailed)
		return nil
	}
	request, err := provider.NewAuthnRequest(state)
	if err != nil {
		p.log().Error("saml login could not build the authentication request", "provider", name, "error", err)
		p.login.Fail(c, federationweb.CodeFailed)
		return nil
	}

	if _, err := p.login.Begin(ctx, c, federationweb.BeginRequest{
		Provider:  name,
		State:     state,
		RequestID: request.ID,
		ReturnTo:  c.Query("return_to"),
	}); err != nil {
		// The store failed, so no transaction exists and no binding cookie was
		// installed: sending the browser to the IdP would only produce a
		// callback that could never be consumed.
		p.log().Error("saml login could not store the transaction", "provider", name, "error", err)
		p.login.Fail(c, federationweb.CodeFailed)
		return nil
	}

	redirect(c, request.URL)
	return nil
}

// acs completes the login. It accepts only the HTTP-POST binding; the route
// registration already enforces that, and the handler repeats the check so a
// directly invoked handler is held to the same contract.
//
// The transaction is consumed immediately after the RelayState names it, before
// the response is decoded or pinned to the request. A replayed ACS request, an
// expired transaction, a request from another browser, and a malformed
// SAMLResponse therefore all burn the transaction they refer to and can never
// be presented again. Only after the neutral service provider has verified the
// signed assertion does the shared base resolve the external identity and hand
// it a new session.
func (p *Plugin) acs(ctx context.Context, c *transportweb.Ctx) error {
	if c == nil || c.Request() == nil {
		p.login.Fail(c, federationweb.CodeBadRequest)
		return nil
	}
	if c.Request().Method != http.MethodPost {
		p.login.Fail(c, federationweb.CodeBadRequest)
		return nil
	}

	name, provider, ok := p.lookup(c)
	if !ok {
		p.login.Fail(c, federationweb.CodeBadRequest)
		return nil
	}

	form, ok := readACSForm(c)
	if !ok {
		p.login.Fail(c, federationweb.CodeBadRequest)
		return nil
	}

	transaction, found, err := p.login.Consume(ctx, c, name, form.relayState)
	if err != nil {
		p.log().Error("saml login could not consume the transaction", "provider", name, "error", err)
		p.login.Fail(c, federationweb.CodeFailed)
		return nil
	}
	if !found {
		// A replay, an expired transaction, a response from another browser,
		// and an unknown RelayState are one outward answer.
		p.login.Fail(c, federationweb.CodeBadRequest)
		return nil
	}

	responseXML, err := decodeSAMLResponse(form.response)
	if err != nil {
		p.log().Info("saml login response could not be decoded", "provider", name, "error", err)
		p.login.Fail(c, federationweb.CodeBadRequest)
		return nil
	}

	// The configured ACS URL is the trust anchor: it is what the response
	// Destination and the bearer Recipient must name, and it never comes from
	// the request Host.
	identity, err := samlcore.Verify(ctx, provider, samlcore.ResponseInput{
		ResponseXML:       responseXML,
		ExpectedRequestID: transaction.RequestID,
		ACSURL:            provider.ACSURL(),
	})
	if err != nil {
		// The neutral module guarantees no assertion content, NameID,
		// attribute value, certificate, or key reaches this error value.
		p.log().Error("saml login response verification failed", "provider", name, "error", err)
		p.login.Fail(c, responseErrorCode(err))
		return nil
	}

	return p.login.Complete(ctx, c, identity, transaction.ReturnTo)
}

// metadata publishes the service-provider metadata document for one provider.
// An unknown or malformed provider, and any method other than GET, is a plain
// 404: metadata is not part of a login, so there is no failure landing to
// redirect to and nothing to echo. The route registration already enforces the
// method, and the handler repeats the check so a directly invoked handler is
// held to the same contract.
func (p *Plugin) metadata(_ context.Context, c *transportweb.Ctx) error {
	if c == nil {
		return nil
	}
	if c.Request() == nil || c.Request().Method != http.MethodGet {
		c.Status(http.StatusNotFound)
		return nil
	}
	name, provider, ok := p.lookup(c)
	if !ok {
		c.Status(http.StatusNotFound)
		return nil
	}
	document, err := provider.Metadata()
	if err != nil {
		p.log().Error("saml metadata could not be rendered", "provider", name, "error", err)
		c.Status(http.StatusInternalServerError)
		return nil
	}
	c.Data(http.StatusOK, metadataContentType, document)
	return nil
}

// acsForm is the pair of fields the HTTP-POST binding carries.
type acsForm struct {
	response   string
	relayState string
}

// readACSForm reads and parses the POST body under the form-size bound. It
// requires exactly one SAMLResponse and one RelayState: a duplicated field is
// an ambiguous request, not something to resolve by taking the first value.
func readACSForm(c *transportweb.Ctx) (acsForm, bool) {
	request := c.Request()
	if request.Body == nil {
		return acsForm{}, false
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxFormBodyBytes+1))
	if err != nil || len(body) > maxFormBodyBytes {
		return acsForm{}, false
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return acsForm{}, false
	}
	responses := values["SAMLResponse"]
	relayStates := values["RelayState"]
	if len(responses) != 1 || len(relayStates) != 1 {
		return acsForm{}, false
	}
	if responses[0] == "" || relayStates[0] == "" {
		return acsForm{}, false
	}
	return acsForm{response: responses[0], relayState: relayStates[0]}, true
}

// errResponseTooLarge reports a decoded SAMLResponse above the parser's limit.
// The reason is deliberately static: the error is logged, never rendered.
var errResponseTooLarge = errors.New("saml response is above the size limit")

// decodeSAMLResponse base64-decodes value while bounding the decoded size. The
// decoder itself rejects any byte outside the standard alphabet, and the limit
// keeps a small base64 string from expanding past the parser's document bound.
func decodeSAMLResponse(value string) ([]byte, error) {
	if value == "" {
		return nil, errors.New("saml response is empty")
	}
	decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(value))
	decoded, err := io.ReadAll(io.LimitReader(decoder, maxDecodedResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(decoded) == 0 || len(decoded) > maxDecodedResponseBytes {
		return nil, errResponseTooLarge
	}
	return decoded, nil
}

// newState returns the 128-bit RelayState one login transaction carries. The
// value is lowercase hexadecimal, so it needs no escaping in the redirect query
// and satisfies the transaction store's unreserved-character rule.
func newState() (string, error) {
	random := make([]byte, stateBytes)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return hex.EncodeToString(random), nil
}

// responseErrorCode maps a verification failure onto the closed outward
// vocabulary. A malformed or replayed assertion is the IdP's material being
// unusable, while a replay cache or another dependency that is unavailable is
// an operational failure. The detailed reason never leaves the log.
func responseErrorCode(err error) federationweb.ErrorCode {
	switch {
	case errors.Is(err, samlcore.ErrReplay), errors.Is(err, samlcore.ErrInvalidResponse):
		return federationweb.CodeBadRequest
	default:
		return federationweb.CodeFailed
	}
}

// lookup resolves the route's provider parameter against the registry. The
// name is validated against the provider-name rule before the registry is
// consulted, and a malformed value is indistinguishable from an unregistered
// one both outward and in the log-free path.
func (p *Plugin) lookup(c *transportweb.Ctx) (string, *samlcore.Provider, bool) {
	if c == nil {
		return "", nil, false
	}
	name := c.Param("provider")
	if !validProviderName(name) {
		return "", nil, false
	}
	provider, ok := p.registry.Get(name)
	if !ok || provider == nil {
		return "", nil, false
	}
	return name, provider, true
}

// validProviderName enforces the same rule federation.Registry applies to
// registry keys: ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$. The rule is duplicated
// rather than imported because the registry keeps its validator unexported;
// the length bound is taken from the contract so the two cannot drift.
func validProviderName(name string) bool {
	if len(name) == 0 || len(name) > federationcore.MaxProviderNameLength {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case index > 0 && (character == '_' || character == '-'):
		default:
			return false
		}
	}
	return true
}

// redirect sends the browser to location without letting any intermediary
// store the response.
func redirect(c *transportweb.Ctx, location string) {
	c.SetHeader("Cache-Control", "no-store")
	c.SetHeader("Location", location)
	c.Status(http.StatusFound)
}
