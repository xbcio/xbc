package federation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	federationcore "github.com/xbcio/xbc/extensions/authentication/federation"
	sessioncore "github.com/xbcio/xbc/extensions/authentication/session"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/transport/web"
	sessionweb "github.com/xbcio/xbc/transport/web/extensions/authentication/session"
)

// ErrorCode is the closed set of error codes a failed federation login may
// report to the browser. The codes are the entire outward vocabulary: no IdP
// text, state value, assertion, or exception text travels with them.
type ErrorCode string

const (
	// CodeDenied reports that the application refused a verified external
	// identity: unbound, disabled, or requiring an explicit binding step.
	CodeDenied ErrorCode = "federation_denied"
	// CodeFailed reports an operational failure: a resolver error, a store
	// failure, or a session operation that did not complete.
	CodeFailed ErrorCode = "federation_failed"
	// CodeBadRequest reports malformed or unusable request material: an
	// unknown provider, a missing or already consumed transaction, or a
	// request that did not come from the browser that started the login.
	CodeBadRequest ErrorCode = "federation_bad_request"
)

func (c ErrorCode) valid() bool {
	switch c {
	case CodeDenied, CodeFailed, CodeBadRequest:
		return true
	default:
		return false
	}
}

// ErrInvalidRequest reports malformed login input or a missing request
// context. An adapter maps it to CodeBadRequest when the input came from the
// browser.
var ErrInvalidRequest = errors.New("federation: invalid login request")

// BeginRequest carries the one-time material a protocol module generated for
// one login. State, Nonce, and CodeVerifier are secrets until the transaction
// is consumed: State travels through the browser, while Nonce and
// CodeVerifier never leave the server except as their derived values.
type BeginRequest struct {
	// Provider is the registered provider identifier.
	Provider string
	// State is the protocol's anti-forgery value and the transaction lookup
	// key. It must carry at least 128 bits of entropy from the protocol
	// module.
	State string
	// Nonce is the OIDC nonce, when the protocol uses one.
	Nonce string
	// CodeVerifier is the PKCE verifier, when the protocol uses PKCE.
	CodeVerifier string
	// RequestID is the SAML AuthnRequest ID, when the protocol is SAML.
	RequestID string
	// ReturnTo is the browser's requested landing location. It is sanitized
	// before it is stored; an unacceptable value silently becomes the
	// configured default.
	ReturnTo string
}

// Login is the shared web login base the OIDC, OAuth2, and SAML adapters
// consume. It owns the login transaction, the return_to decision, and the
// session handoff; protocol modules own everything that is protocol-specific.
type Login interface {
	// Begin stores one login transaction and installs the browser-binding
	// cookie. The adapter then redirects the browser to the protocol module's
	// authorization URL.
	Begin(ctx context.Context, c *web.Ctx, request BeginRequest) (Transaction, error)
	// Consume atomically destroys the transaction named by provider and state
	// and returns it only when the callback arrives from the browser that
	// started the login.
	Consume(ctx context.Context, c *web.Ctx, provider, state string) (Transaction, bool, error)
	// Complete resolves the verified external identity and either creates the
	// session and redirects to returnTo, or redirects to the failure landing
	// with a fixed error code.
	Complete(ctx context.Context, c *web.Ctx, identity federationcore.ExternalIdentity, returnTo string) error
	// Fail redirects to the configured failure landing with code.
	Fail(c *web.Ctx, code ErrorCode)
	// ReturnTo sanitizes raw through the configured policy.
	ReturnTo(raw string) string
	// SetIfAbsent reserves namespace/key for ttl; the SAML adapter uses it as
	// its assertion replay cache.
	SetIfAbsent(ctx context.Context, namespace, key string, ttl time.Duration) (bool, error)
}

// Plugin is the shared federation web base. It starts no goroutine, owns no
// connection, and performs no asynchronous work, so it implements neither
// plugin.Drainer nor Stop; the Redis client it borrows belongs to the plugin
// instance that supplied it.
type Plugin struct {
	config        normalizedConfig
	resolver      federationcore.IdentityResolver
	sessions      sessionweb.Manager
	sessionCookie string
	store         Store
	now           func() time.Time
	logger        log.Logger
	random        io.Reader
}

var _ Login = (*Plugin)(nil)

func (p *Plugin) clock() time.Time {
	if p.now == nil {
		return time.Now().UTC()
	}
	return p.now().UTC()
}

func (p *Plugin) log() log.Logger {
	if p.logger == nil {
		return log.L()
	}
	return p.logger
}

// Begin stores request's transaction and installs the binding cookie. A
// store failure leaves both the store and the browser unchanged: the cookie is
// installed only after the transaction is durable.
func (p *Plugin) Begin(ctx context.Context, c *web.Ctx, request BeginRequest) (Transaction, error) {
	if p == nil {
		return Transaction{}, errors.New("federation: nil login plugin")
	}
	if err := checkContext(ctx); err != nil {
		return Transaction{}, err
	}
	if c == nil || c.Request() == nil || c.Writer() == nil {
		return Transaction{}, fmt.Errorf("%w: request context is unavailable", ErrInvalidRequest)
	}
	if !validProviderName(request.Provider) || !validState(request.State) {
		return Transaction{}, fmt.Errorf("%w: provider or state is invalid", ErrInvalidRequest)
	}

	secret, err := randomToken(p.random, bindingSecretBytes)
	if err != nil {
		return Transaction{}, err
	}
	now := p.clock()
	transaction := Transaction{
		Provider:     request.Provider,
		State:        request.State,
		Nonce:        request.Nonce,
		CodeVerifier: request.CodeVerifier,
		RequestID:    request.RequestID,
		ReturnTo:     p.config.returnTo.sanitize(request.ReturnTo),
		Binding:      bindingDigest(secret),
		ExpiresAt:    now.Add(p.config.transactionTTL),
	}
	if err := p.store.Create(ctx, transaction); err != nil {
		return Transaction{}, err
	}
	p.installBindingCookie(c, secret, now.Add(p.config.transactionTTL))
	return transaction, nil
}

// Consume destroys the transaction and returns it only to the browser that
// started the login. A missing cookie, a different browser, an expired
// transaction, and a replayed callback are all reported as found == false.
func (p *Plugin) Consume(ctx context.Context, c *web.Ctx, provider, state string) (Transaction, bool, error) {
	if p == nil {
		return Transaction{}, false, errors.New("federation: nil login plugin")
	}
	if err := checkContext(ctx); err != nil {
		return Transaction{}, false, err
	}
	if c == nil || c.Request() == nil {
		return Transaction{}, false, fmt.Errorf("%w: request context is unavailable", ErrInvalidRequest)
	}
	if !validProviderName(provider) || !validState(state) {
		return Transaction{}, false, nil
	}

	secret := ""
	if cookies := c.Request().CookiesNamed(p.config.cookieName); len(cookies) == 1 && cookies[0] != nil {
		secret = cookies[0].Value
	}
	p.clearBindingCookie(c)
	if secret == "" {
		return Transaction{}, false, nil
	}

	transaction, found, err := p.store.Consume(ctx, provider, state, bindingDigest(secret))
	if err != nil {
		return Transaction{}, false, err
	}
	if !found || !transaction.ExpiresAt.After(p.clock()) {
		return Transaction{}, false, nil
	}
	return transaction, true, nil
}

// Complete performs the session handoff for an already verified external
// identity. It never returns an operational failure: every failure is logged
// internally and rendered as the configured failure landing, so a handler
// that returns Complete's result cannot produce a second response.
//
// A session presented by the request is revoked before the new one is
// created, which is what prevents session fixation: the ID the browser held
// before the login never survives it. The new session is created, not rotated:
// rotation would preserve the previous absolute lifetime, and a login starts a
// fresh one.
func (p *Plugin) Complete(ctx context.Context, c *web.Ctx, identity federationcore.ExternalIdentity, returnTo string) error {
	if p == nil {
		return errors.New("federation: nil login plugin")
	}
	if err := checkContext(ctx); err != nil {
		return err
	}
	if c == nil || c.Writer() == nil {
		return fmt.Errorf("%w: request context is unavailable", ErrInvalidRequest)
	}

	resolution, err := federationcore.ResolveIdentity(ctx, p.resolver, identity)
	if err != nil {
		p.log().Error("federation login resolution failed",
			"provider", identity.Provider, "protocol", identity.Protocol, "error", err)
		p.Fail(c, CodeFailed)
		return nil
	}
	if reason, refused := resolution.Refusal(); refused {
		p.log().Info("federation login refused",
			"provider", identity.Provider, "protocol", identity.Protocol, "reason", string(reason))
		p.Fail(c, CodeDenied)
		return nil
	}

	attributes, err := federationcore.SessionAttributes(identity, resolution.Attributes())
	if err != nil {
		p.log().Error("federation login attributes are unusable",
			"provider", identity.Provider, "protocol", identity.Protocol, "error", err)
		p.Fail(c, CodeFailed)
		return nil
	}

	if err := p.revokePresentedSessions(ctx, c); err != nil {
		p.log().Error("federation login could not revoke the presented session",
			"provider", identity.Provider, "protocol", identity.Protocol, "error", err)
		p.Fail(c, CodeFailed)
		return nil
	}

	value, err := p.sessions.Create(ctx, resolution.Subject(), attributes)
	if err != nil {
		p.log().Error("federation login could not create a session",
			"provider", identity.Provider, "protocol", identity.Protocol, "error", err)
		p.Fail(c, CodeFailed)
		return nil
	}
	if err := p.sessions.SetCookie(c, value); err != nil {
		if revokeErr := p.sessions.Revoke(ctx, value.ID); revokeErr != nil && !ignorableRevokeError(revokeErr) {
			p.log().Error("federation login could not revoke an unusable new session",
				"provider", identity.Provider, "protocol", identity.Protocol, "error", revokeErr)
		}
		p.log().Error("federation login could not install the session cookie",
			"provider", identity.Provider, "protocol", identity.Protocol, "error", err)
		p.Fail(c, CodeFailed)
		return nil
	}

	redirect(c, p.config.returnTo.sanitize(returnTo))
	return nil
}

// revokePresentedSessions revokes every session the request presented under
// the configured session cookie name. A malformed or unknown value cannot name
// a live session and is ignored; a store failure is returned so the login does
// not leave a fixated session alive.
func (p *Plugin) revokePresentedSessions(ctx context.Context, c *web.Ctx) error {
	if c.Request() == nil {
		return nil
	}
	seen := make(map[string]bool)
	for _, cookie := range c.Request().CookiesNamed(p.sessionCookie) {
		if cookie == nil || cookie.Value == "" || seen[cookie.Value] {
			continue
		}
		seen[cookie.Value] = true
		if err := p.sessions.Revoke(ctx, cookie.Value); err != nil {
			if ignorableRevokeError(err) {
				continue
			}
			return err
		}
	}
	return nil
}

func ignorableRevokeError(err error) bool {
	return errors.Is(err, sessioncore.ErrInvalidCookie) || errors.Is(err, sessioncore.ErrNotFound)
}

// ReturnTo sanitizes raw through the configured policy. An unacceptable value
// becomes the configured default.
func (p *Plugin) ReturnTo(raw string) string {
	if p == nil {
		return "/"
	}
	return p.config.returnTo.sanitize(raw)
}

// Fail redirects to the configured failure landing with code. An unknown code
// is collapsed to CodeFailed so an adapter cannot widen the outward
// vocabulary.
func (p *Plugin) Fail(c *web.Ctx, code ErrorCode) {
	if p == nil || c == nil || c.Writer() == nil {
		return
	}
	if !code.valid() {
		code = CodeFailed
	}
	redirect(c, p.failureLocation(code))
}

// SetIfAbsent reserves namespace/key for ttl and reports whether this call
// created it.
func (p *Plugin) SetIfAbsent(ctx context.Context, namespace, key string, ttl time.Duration) (bool, error) {
	if p == nil {
		return false, errors.New("federation: nil login plugin")
	}
	if err := checkContext(ctx); err != nil {
		return false, err
	}
	return p.store.SetIfAbsent(ctx, namespace, key, ttl)
}

// failureLocation builds the failure landing with only the fixed error code
// attached. The failure path was validated at configuration time, so it is
// always a same-origin relative path.
func (p *Plugin) failureLocation(code ErrorCode) string {
	target, err := url.Parse(p.config.failurePath)
	if err != nil {
		return p.config.failurePath
	}
	query := target.Query()
	query.Set("error", string(code))
	target.RawQuery = query.Encode()
	return target.String()
}

func (p *Plugin) installBindingCookie(c *web.Ctx, secret string, expires time.Time) {
	remaining := expires.Sub(p.clock())
	maxAge := int((remaining + time.Second - 1) / time.Second)
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(c.Writer(), &http.Cookie{
		Name:     p.config.cookieName,
		Value:    secret,
		Path:     p.config.cookiePath,
		Domain:   p.config.cookieDomain,
		Expires:  expires,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   p.config.cookieSecure,
		SameSite: p.config.cookieSameSite,
	})
}

func (p *Plugin) clearBindingCookie(c *web.Ctx) {
	if c == nil || c.Writer() == nil {
		return
	}
	http.SetCookie(c.Writer(), &http.Cookie{
		Name:     p.config.cookieName,
		Value:    "",
		Path:     p.config.cookiePath,
		Domain:   p.config.cookieDomain,
		Expires:  time.Unix(1, 0).UTC(),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   p.config.cookieSecure,
		SameSite: p.config.cookieSameSite,
	})
}

// redirect sends the browser to location without letting any intermediary
// store the response.
func redirect(c *web.Ctx, location string) {
	c.SetHeader("Cache-Control", "no-store")
	c.SetHeader("Location", location)
	c.Status(http.StatusFound)
}
