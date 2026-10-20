package csrf

import (
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is the stable configuration and middleware identity.
const Key plugin.Key = "csrf"

// Plugin contributes the CSRF middleware to transport/web. Its compiled policy
// is immutable for the plugin's whole lifetime, so one instance serves
// concurrent requests without locks.
type Plugin struct {
	policy *policy
}

var _ web.Middleware = (*Plugin)(nil)

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	func(_ plugin.BuildContext, cfg Config) (*Plugin, error) {
		return New(cfg)
	},
	plugin.Options[*Plugin]{
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.Middleware](func(value *Plugin) web.Middleware { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// New constructs directly usable CSRF middleware from cfg.
func New(cfg Config) (*Plugin, error) {
	compiled, err := compilePolicy(cfg)
	if err != nil {
		return nil, err
	}
	return &Plugin{policy: compiled}, nil
}

func prepareConfig(cfg Config) (Config, error) {
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Handler returns the middleware handler.
func (p *Plugin) Handler() web.Handler { return p.handle }

// Order places CSRF protection in the request-security phase. Security headers
// prefer to run before it, so a refused unsafe request still carries the
// framework's response headers.
func (*Plugin) Order() web.Order {
	return web.Order{Phase: web.PhaseSecurity}
}

// Definition returns CSRF's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns CSRF's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }

// TokenFor returns the CSRF token bound to sessionID, or "" unless the plugin
// runs in token mode with a configured secret.
//
// It is the login-response hook: the session plugin's SetCookie writes the new
// session cookie to the response and never mutates the login request, so a
// login handler must pass the session it just installed -- TokenFor(value.ID) --
// rather than calling Token on the request. The token is meaningful only in
// token mode; calling TokenFor (or Token) unconditionally in origin mode is
// harmless and returns "".
//
// The token is a base64url HMAC-SHA256 of sessionID under the configured
// secret. It is never persisted: rotating the session identifier invalidates
// every token issued for the old one.
func (p *Plugin) TokenFor(sessionID string) string {
	if p == nil || p.policy == nil || p.policy.mode != modeToken || len(p.policy.secret) == 0 {
		return ""
	}
	if sessionID == "" || len(sessionID) > maxSessionCookieBytes {
		return ""
	}
	return p.policy.tokenString(sessionID)
}

// Token returns the CSRF token for the session named by the current request's
// session cookie, or "" when that cookie is absent, duplicated, empty, or
// larger than 4 KiB. It is for a request that already carries the cookie.
// A login response must use TokenFor instead: SetCookie writes the cookie to
// the response, so the login request itself never carries it.
//
// The token is a base64url HMAC-SHA256 of the cookie's raw value under the
// configured secret. It is meaningful only in token mode -- the middleware
// never reads it in origin mode -- and it is never persisted: rotating the
// session identifier invalidates every token issued for the old one.
func (p *Plugin) Token(c *web.Ctx) string {
	if p == nil || p.policy == nil || c == nil {
		return ""
	}
	request := c.Request()
	if request == nil {
		return ""
	}
	sessionID, ok := p.policy.sessionID(request)
	if !ok {
		return ""
	}
	return p.TokenFor(sessionID)
}
