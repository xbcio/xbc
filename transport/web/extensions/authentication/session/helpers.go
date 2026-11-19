package session

import (
	"context"
	"errors"
	"strings"

	"github.com/xbcio/xbc/extensions/authentication"
	sessioncore "github.com/xbcio/xbc/extensions/authentication/session"
	"github.com/xbcio/xbc/transport/web"
)

const (
	// SessionIDAttribute is the principal attribute the neutral session
	// authenticator publishes under the verified session's opaque ID. It is the
	// only principal attribute this package reads.
	SessionIDAttribute = "session_id"
)

var (
	// ErrUnavailable reports a nil adapter or a nil neutral session plugin.
	ErrUnavailable = errors.New("session-http: session manager is unavailable")
	// ErrRequestUnavailable reports a helper call without a request or a
	// response writer.
	ErrRequestUnavailable = errors.New("session-http: request is unavailable")
	// ErrContextUnavailable reports a helper call without a context.
	ErrContextUnavailable = errors.New("session-http: context is unavailable")
	// ErrNoCurrentSession reports a refresh or patch with no verified current
	// session and no usable presented credential.
	ErrNoCurrentSession = errors.New("session-http: no current session")
)

// Handler is the request-scoped session helper set layered over Manager.
//
// Login and Logout implement the login lifecycle a Web application used to
// assemble by hand: create/revoke through the neutral manager, then move the
// opaque ID through the writable carrier. Refresh and Patch update the current
// session's attributes without rotating it. Every decision -- session shape,
// lifetime, max_sessions, and verification -- stays with the neutral manager;
// this type only resolves the current request's session ID and moves the
// credential.
//
// The Definition does not export Handler as a separate contract, so resolving
// these helpers from the plugin graph requires the primary value:
//
//	var sessions = plugin.RefTo[*session.Plugin](session.Key)
type Handler interface {
	// Login creates a session for subject and installs its opaque ID through
	// the writable carrier. A request that already presents a valid session has
	// that session revoked first (the anti-fixation step a session-aware login
	// performs), so the old ID stops working. A malformed or unknown presented
	// ID needs no revocation and still logs in. When creating the session is
	// refused with the neutral manager's ErrTooManySessions, that error is
	// returned unchanged and no credential is written. When writing the
	// credential fails, the new session is revoked before the error is
	// returned, so a failed login leaves no live session and no new cookie.
	Login(ctx context.Context, c *web.Ctx, subject string, attributes map[string]any) (sessioncore.Session, error)

	// Logout revokes the session the request presents and clears the
	// credential through the writable carrier. The session ID comes from the
	// verified principal when the authentication middleware published one, and
	// from the carrier chain otherwise. An absent, malformed, or already
	// revoked session is not an error: logout is idempotent. A store failure is
	// returned after the credential is cleared, so the client stops presenting
	// a session the server could not retire.
	Logout(ctx context.Context, c *web.Ctx) error

	// Refresh replaces the current request's session attributes in place,
	// using AttributeManager.UpdateAttributes' whole-map semantics. It
	// preserves the ID, subject, expiry, and idle lease, and does not revive a
	// dead session.
	Refresh(ctx context.Context, c *web.Ctx, attributes map[string]any) (sessioncore.Session, error)

	// Patch applies an attribute delta to the current request's session, using
	// AttributeManager.PatchAttributes' set/unset semantics, so concurrent
	// patches of different keys do not overwrite each other.
	Patch(ctx context.Context, c *web.Ctx, set map[string]any, unset []string) (sessioncore.Session, error)
}

// Login creates a session for subject and installs its opaque ID through the
// writable carrier. See Handler.Login for the anti-fixation, limit, and
// rollback rules.
func (p *Plugin) Login(
	ctx context.Context,
	c *web.Ctx,
	subject string,
	attributes map[string]any,
) (sessioncore.Session, error) {
	if err := p.ready(ctx, c); err != nil {
		return sessioncore.Session{}, err
	}
	if p.writer == nil {
		return sessioncore.Session{}, ErrNoWritableCarrier
	}
	if err := p.revokePresented(ctx, c); err != nil {
		return sessioncore.Session{}, err
	}
	value, err := p.core.Create(ctx, subject, attributes)
	if err != nil {
		return sessioncore.Session{}, err
	}
	if err := p.writer.Issue(c, value); err != nil {
		// The credential never reached the client, so the session must not stay
		// live behind an unusable response.
		_ = p.core.Revoke(ctx, value.ID)
		return sessioncore.Session{}, err
	}
	return value, nil
}

// Logout revokes the presented session and clears the writable carrier. See
// Handler.Logout for the idempotency rule.
func (p *Plugin) Logout(ctx context.Context, c *web.Ctx) error {
	if err := p.ready(ctx, c); err != nil {
		return err
	}
	var revokeErr error
	if id, ok := p.candidateID(c); ok {
		err := p.core.Revoke(ctx, id)
		if err != nil && !errors.Is(err, sessioncore.ErrNotFound) && !errors.Is(err, sessioncore.ErrInvalidCookie) {
			revokeErr = err
		}
	}
	if p.writer != nil {
		p.writer.Clear(c)
	}
	return revokeErr
}

// Refresh replaces the current session's attributes in place. See
// Handler.Refresh.
func (p *Plugin) Refresh(
	ctx context.Context,
	c *web.Ctx,
	attributes map[string]any,
) (sessioncore.Session, error) {
	if err := p.ready(ctx, c); err != nil {
		return sessioncore.Session{}, err
	}
	id, ok := p.currentID(ctx, c)
	if !ok {
		return sessioncore.Session{}, ErrNoCurrentSession
	}
	return p.extended.UpdateAttributes(ctx, id, attributes)
}

// Patch applies an attribute delta to the current session. See Handler.Patch.
func (p *Plugin) Patch(
	ctx context.Context,
	c *web.Ctx,
	set map[string]any,
	unset []string,
) (sessioncore.Session, error) {
	if err := p.ready(ctx, c); err != nil {
		return sessioncore.Session{}, err
	}
	id, ok := p.currentID(ctx, c)
	if !ok {
		return sessioncore.Session{}, ErrNoCurrentSession
	}
	return p.extended.PatchAttributes(ctx, id, set, unset)
}

// ready performs the guards every helper shares: the adapter and its neutral
// plugin exist, the request and response writer exist, and the caller supplied
// a context.
func (p *Plugin) ready(ctx context.Context, c *web.Ctx) error {
	if p == nil || p.core == nil || p.extended == nil || p.Manager == nil {
		return ErrUnavailable
	}
	if ctx == nil {
		return ErrContextUnavailable
	}
	if c == nil || c.Request() == nil || c.Writer() == nil {
		return ErrRequestUnavailable
	}
	return nil
}

// revokePresented revokes a valid session the request already presents. A
// malformed or unknown value is not an error: there is nothing live to revoke,
// and the login still proceeds with a fresh ID.
func (p *Plugin) revokePresented(ctx context.Context, c *web.Ctx) error {
	result := p.carriers.Extract(c)
	if result.Status() != authentication.CredentialStatusPresented {
		return nil
	}
	credential, ok := result.Credential()
	if !ok {
		return nil
	}
	id, ok := credential.Value().(string)
	if !ok || id == "" {
		return nil
	}
	_, verdict, err := p.extended.Verify(ctx, id)
	if err != nil || verdict != sessioncore.VerdictValid {
		return nil
	}
	if err := p.core.Revoke(ctx, id); err != nil && !errors.Is(err, sessioncore.ErrNotFound) {
		return err
	}
	return nil
}

// candidateID resolves the session ID Logout should revoke without verifying
// it: a revocation of an unknown or malformed ID is naturally idempotent, and
// asking the store twice would only add a failure mode between the two calls.
func (p *Plugin) candidateID(c *web.Ctx) (string, bool) {
	if id, ok := CurrentID(c); ok {
		return id, true
	}
	if p.carriers == nil {
		return "", false
	}
	result := p.carriers.Extract(c)
	if result.Status() != authentication.CredentialStatusPresented {
		return "", false
	}
	credential, ok := result.Credential()
	if !ok {
		return "", false
	}
	id, ok := credential.Value().(string)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// currentID resolves the current request's verified session ID: from the
// principal the authentication middleware published when there is one, and
// otherwise by extracting and verifying the presented credential directly. The
// second path lets these helpers run on a public route, such as login, that the
// middleware does not authenticate.
func (p *Plugin) currentID(ctx context.Context, c *web.Ctx) (string, bool) {
	if id, ok := CurrentID(c); ok {
		return id, true
	}
	if p.carriers == nil {
		return "", false
	}
	result := p.carriers.Extract(c)
	if result.Status() != authentication.CredentialStatusPresented {
		return "", false
	}
	credential, ok := result.Credential()
	if !ok {
		return "", false
	}
	id, ok := credential.Value().(string)
	if !ok || id == "" {
		return "", false
	}
	_, verdict, err := p.extended.Verify(ctx, id)
	if err != nil || verdict != sessioncore.VerdictValid {
		return "", false
	}
	return id, true
}

// CurrentSession is the read-only view of the authenticated session behind the
// current request. It deliberately carries no session ID: the ID is a bearer
// credential, and a convenience view that includes it invites logging it. Use
// CurrentID when the ID itself is required. Attributes is a defensive copy
// that never includes the session_id bookkeeping entry.
type CurrentSession struct {
	Subject    string
	Attributes map[string]any
}

// Current returns the authenticated session view published by the Web
// authentication middleware. It reports false when the request has no
// principal, for example on a public route or a rejected request.
func Current(c *web.Ctx) (CurrentSession, bool) {
	principal, ok := web.CurrentPrincipal(c)
	if !ok {
		return CurrentSession{}, false
	}
	attributes := make(map[string]any, len(principal.Attributes))
	for key, value := range principal.Attributes {
		if key == SessionIDAttribute {
			continue
		}
		attributes[key] = value
	}
	return CurrentSession{Subject: principal.Subject, Attributes: attributes}, true
}

// CurrentID returns the verified session's opaque ID from the principal. It is
// the explicit accessor for the one sensitive value Current omits. The ID must
// never reach a log, an error message, or a response body.
func CurrentID(c *web.Ctx) (string, bool) {
	principal, ok := web.CurrentPrincipal(c)
	if !ok {
		return "", false
	}
	id, ok := principal.Attributes[SessionIDAttribute].(string)
	if !ok || strings.TrimSpace(id) == "" {
		return "", false
	}
	return id, true
}
