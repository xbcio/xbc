package session

import (
	"errors"
	"net/http"
	"time"

	sessioncore "github.com/xbcio/xbc/extensions/authentication/session"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is the stable Definition identity of the Web cookie adapter. It is
// distinct from the neutral session plugin's own key, session, which owns the
// plugins.session section: the adapter has no configuration of its own and
// reads its cookie settings from that plugin.
const Key plugin.Key = "session-http"

// Manager is the handler-facing session API for Web applications: the neutral
// lifecycle plus installing and clearing the opaque session cookie. Create and
// Rotate return the value whose ID SetCookie installs.
type Manager interface {
	sessioncore.Manager
	SetCookie(c *web.Ctx, value sessioncore.Session) error
	ClearCookie(c *web.Ctx)
}

// Plugin carries session IDs in an HTTP cookie for the neutral session
// authenticator: it extracts the ID from the request and installs or clears
// the cookie for handlers. Every session-lifecycle and verification decision
// stays with the neutral plugin.
type Plugin struct {
	sessioncore.Manager
	core   *sessioncore.Plugin
	cookie sessioncore.CookieSettings
}

var (
	_ web.CredentialExtractor = (*Plugin)(nil)
	_ Manager                 = (*Plugin)(nil)
)

var sessionInput = plugin.RefTo[*sessioncore.Plugin](sessioncore.Key)

var definition = plugin.Define(
	Key,
	func(ctx plugin.BuildContext) (*Plugin, error) {
		return New(sessionInput.Get(ctx).Value)
	},
	plugin.Options[*Plugin]{
		// The adapter follows the session plugin it pairs with: configuring
		// plugins.session turns both on, and leaving it out leaves both dormant.
		Activation: plugin.WhenConfigured("plugins." + sessioncore.Key.String()),
		Inputs:     plugin.Inputs(sessionInput),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.CredentialExtractor](func(value *Plugin) web.CredentialExtractor { return value }),
			plugin.ExportAs(func(value *Plugin) Manager { return value }),
		),
	},
)

// bundle selects the neutral session plugin alongside the adapter. An adapter
// without the session plugin it feeds is not a composition anyone wants, and
// RefTo would reject it at planning time anyway.
var bundle = plugin.CombineBundles(sessioncore.Bundle(), plugin.BundleOf(definition))

// New builds a cookie adapter over the neutral session plugin.
func New(core *sessioncore.Plugin) (*Plugin, error) {
	if core == nil {
		return nil, errors.New("session-http: requires the session plugin")
	}
	return &Plugin{Manager: core.Manager(), core: core, cookie: core.Cookie()}, nil
}

// Definition returns the Web adapter's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the side-effect-free explicit composition of the Web adapter
// and the neutral session plugin it feeds.
func Bundle() plugin.Bundle { return bundle }

// SetCookie installs value's opaque ID as the configured session cookie. It
// refuses a stopped session plugin, a malformed ID, and an expired session.
func (p *Plugin) SetCookie(c *web.Ctx, value sessioncore.Session) error {
	remaining, err := p.core.Lifetime(value)
	if err != nil {
		return err
	}
	if c == nil || c.Writer() == nil {
		return errors.New("session: cookie context cannot be nil")
	}
	maxAge := int((remaining + time.Second - 1) / time.Second)
	http.SetCookie(c.Writer(), &http.Cookie{
		Name:     p.cookie.Name,
		Value:    value.ID,
		Path:     p.cookie.Path,
		Domain:   p.cookie.Domain,
		Expires:  value.ExpiresAt,
		MaxAge:   maxAge,
		HttpOnly: p.cookie.HTTPOnly,
		Secure:   p.cookie.Secure,
		SameSite: p.cookie.SameSite,
	})
	return nil
}

// ClearCookie expires the configured session cookie on the client. It does not
// revoke the server-side session; call Revoke for that.
func (p *Plugin) ClearCookie(c *web.Ctx) {
	if c == nil || c.Writer() == nil {
		return
	}
	http.SetCookie(c.Writer(), &http.Cookie{
		Name:     p.cookie.Name,
		Value:    "",
		Path:     p.cookie.Path,
		Domain:   p.cookie.Domain,
		Expires:  time.Unix(1, 0).UTC(),
		MaxAge:   -1,
		HttpOnly: p.cookie.HTTPOnly,
		Secure:   p.cookie.Secure,
		SameSite: p.cookie.SameSite,
	})
}
