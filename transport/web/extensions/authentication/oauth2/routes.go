package oauth2

import (
	"context"
	"net/http"

	federationcore "github.com/xbcio/xbc/extensions/authentication/federation"
	oauth2core "github.com/xbcio/xbc/extensions/authentication/oauth2"
	transportweb "github.com/xbcio/xbc/transport/web"
	federationweb "github.com/xbcio/xbc/transport/web/extensions/authentication/federation"
)

// RegisterRoutes implements web.RouteContributor.
//
// Both routes are explicitly public: they are the entry points of a login, so
// the browser reaching them has no session yet by definition. Every
// authentication decision happens after the callback has been verified, when
// the shared federation base hands the resolved identity to a new session.
//
// The routes live under the configured prefix and take the provider name as a
// path parameter. A malformed or unregistered provider is answered with the
// same fixed failure code and is never echoed back.
func (p *Plugin) RegisterRoutes(router *transportweb.Router) {
	group := router.Group(p.config.prefix)
	group.GET("/:provider/start", p.start).
		Name("oauth2.start").
		Auth(transportweb.Public())
	group.GET("/:provider/callback", p.callback).
		Name("oauth2.callback").
		Auth(transportweb.Public())
}

// start begins one authorization-code login: the neutral client produces the
// state, PKCE verifier, and authorization URL, the shared base stores the
// transaction bound to this browser, and the browser is sent to the IdP.
func (p *Plugin) start(ctx context.Context, c *transportweb.Ctx) error {
	name, provider, ok := p.lookup(c)
	if !ok {
		p.login.Fail(c, federationweb.CodeBadRequest)
		return nil
	}

	authorization, err := provider.Begin()
	if err != nil {
		p.log().Error("oauth2 login could not be started", "provider", name, "error", err)
		p.login.Fail(c, federationweb.CodeFailed)
		return nil
	}

	if _, err := p.login.Begin(ctx, c, federationweb.BeginRequest{
		Provider:     name,
		State:        authorization.State,
		CodeVerifier: authorization.CodeVerifier,
		ReturnTo:     c.Query("return_to"),
	}); err != nil {
		// The store failed, so no transaction exists and no binding cookie was
		// installed: sending the browser to the IdP would only produce a
		// callback that could never be consumed.
		p.log().Error("oauth2 login could not store the transaction", "provider", name, "error", err)
		p.login.Fail(c, federationweb.CodeFailed)
		return nil
	}

	redirect(c, authorization.URL)
	return nil
}

// callback completes the login. The transaction is consumed before anything
// else is decided, so a callback can never be replayed and an IdP error
// response still burns the transaction it belongs to. Only after the neutral
// client has exchanged the code and read the account document does the shared
// base resolve the external identity and hand it a new session.
func (p *Plugin) callback(ctx context.Context, c *transportweb.Ctx) error {
	name, provider, ok := p.lookup(c)
	if !ok {
		p.login.Fail(c, federationweb.CodeBadRequest)
		return nil
	}

	transaction, found, err := p.login.Consume(ctx, c, name, c.Query("state"))
	if err != nil {
		p.log().Error("oauth2 login could not consume the transaction", "provider", name, "error", err)
		p.login.Fail(c, federationweb.CodeFailed)
		return nil
	}
	if !found {
		// A replay, an expired transaction, a callback from another browser,
		// and an unknown state are one outward answer.
		p.login.Fail(c, federationweb.CodeBadRequest)
		return nil
	}

	if raw := c.Query("error"); raw != "" {
		code := idpErrorCode(raw)
		p.log().Info("oauth2 provider reported a login failure", "provider", name, "idp_error", code)
		if code == "access_denied" {
			p.login.Fail(c, federationweb.CodeDenied)
		} else {
			p.login.Fail(c, federationweb.CodeFailed)
		}
		return nil
	}

	code := c.Query("code")
	if code == "" {
		p.login.Fail(c, federationweb.CodeBadRequest)
		return nil
	}

	identity, err := provider.Exchange(ctx, code, oauth2core.AuthorizationRequest{
		State:        transaction.State,
		CodeVerifier: transaction.CodeVerifier,
	})
	if err != nil {
		// The detailed reason is safe to log: the neutral client guarantees no
		// credential, token, or IdP response body reaches an error value.
		p.log().Error("oauth2 login exchange failed", "provider", name, "error", err)
		p.login.Fail(c, federationweb.CodeFailed)
		return nil
	}

	return p.login.Complete(ctx, c, identity, transaction.ReturnTo)
}

// lookup resolves the route's provider parameter against the registry. The
// name is validated against the provider-name rule before the registry is
// consulted, and a malformed value is indistinguishable from an unregistered
// one both outward and in the log-free path.
func (p *Plugin) lookup(c *transportweb.Ctx) (string, *oauth2core.Provider, bool) {
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

// idpErrorCode returns value only when it is a plausible OAuth 2.0 error code.
// The query parameter is attacker-controlled, so it reaches a log record only
// after this filter: a value carrying separators, newlines, or markup is
// dropped instead of being written verbatim.
func idpErrorCode(value string) string {
	if len(value) == 0 || len(value) > 64 {
		return ""
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '_' || character == '-' || character == '.':
		default:
			return ""
		}
	}
	return value
}

// redirect sends the browser to location without letting any intermediary
// store the response.
func redirect(c *transportweb.Ctx, location string) {
	c.SetHeader("Cache-Control", "no-store")
	c.SetHeader("Location", location)
	c.Status(http.StatusFound)
}
