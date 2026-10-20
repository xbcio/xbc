// Package csrf provides an XBC Web middleware plugin that rejects cross-site
// state-changing requests. It is dormant until configured and runs in the
// request-security phase beside CORS.
//
// # Modes
//
// Origin mode (the default) admits only requests whose Origin -- or, when
// Origin is absent, whose Referer origin -- is the request's own origin or
// exactly one entry of allow_origins. A present Sec-Fetch-Site header must
// agree: "same-origin" and "none" defer to that origin decision, while
// "same-site" and "cross-site" require an explicit allowlist match. A request
// carrying neither Origin nor Referer is refused rather than admitted by
// default.
//
// Token mode requires exactly one session cookie and a token header, and
// verifies that the header is base64url(HMAC-SHA256(secret, session cookie
// value)). Rotating the session identifier therefore invalidates every token
// issued for the old one, and no server-side token store exists. A duplicated
// session cookie is refused rather than read first-wins, matching the session
// extractor's rule. The secret is required, must be at least 32 bytes, and is
// never a source constant.
//
// Plugin.Token returns the token for a request that already carries the
// session cookie. A login response uses Plugin.TokenFor(value.ID) with the
// sessioncore.Session the handler just installed instead: the session plugin's
// SetCookie writes the new cookie to the response and does not mutate the login
// request, so Token cannot see that session on the response that installs it.
// Both methods return "" in origin mode and when no secret is configured, so
// an unconditional call is harmless. The frontend keeps the token in memory and
// echoes it in the configured header.
//
// # Reverse proxies
//
// Origin mode derives the request's own scheme from request.TLS. Behind a
// TLS-terminating proxy request.TLS is nil, so a browser's https://host Origin
// is not same-origin from this process's point of view: list the public https
// origin in allow_origins (an exact allowlist match) or terminate TLS
// in-process. This package deliberately has no trust-forwarded-proto option.
//
// # Exemptions
//
// exclude_paths is matched with path.Match against both the frozen route
// template and the concrete request path, so one pattern covers every path a
// parameterized route matches. exclude_routes matches the frozen route name.
// Exempting health probes and third-party OAuth/OIDC/SAML callback paths is the
// documented use. Exemption is per route and never a global switch: there is
// deliberately no option that disables the middleware, because a CSRF plugin
// that can be turned off globally is the vulnerability it exists to prevent.
//
// # Session cookie coupling
//
// session_cookie defaults to "xbc_session", the session plugin's default name.
// The coupling is by name only: this package does not depend on the session
// plugin and never reads the cookie's meaning. Change the name here when the
// application configures a different session cookie name.
//
// # Usage
//
// Compose CSRF explicitly with the Web prelude and answer login requests with
// the token in token mode:
//
//	import (
//		"github.com/xbcio/xbc"
//		webprelude "github.com/xbcio/xbc/transport/web/prelude"
//		ginengine "github.com/xbcio/xbc/transport/web/engines/gin"
//		csrf "github.com/xbcio/xbc/transport/web/extensions/security/csrf"
//	)
//
//	func newApp() (*xbc.App, error) {
//		return xbc.New(xbc.WithBundles(
//			webprelude.Bundle(),
//			ginengine.Bundle(),
//			csrf.Bundle(),
//		))
//	}
//
// A plugins.csrf section activates the composed Definition. Origin mode is
// enough for a same-origin application; token mode additionally needs a
// deployment-supplied secret:
//
//	plugins:
//	  csrf:
//	    mode: origin
//	    allow_origins: ["https://admin.example.com"]
//	    exclude_paths: ["/healthz", "/readyz", "/auth/*/callback"]
//	    exclude_routes: ["health.live", "health.ready"]
//
// In token mode the login handler answers with the token for the session it
// just installed, using TokenFor rather than Token. The plugin's primary
// concrete type is resolvable, so a login plugin can hold it directly:
//
//	var csrfPlugin = plugin.RefTo[*csrf.Plugin](csrf.Key)
//
//	func (h *Login) login(ctx context.Context, c *web.Ctx) error {
//		value, err := h.sessions.Create(ctx, principal.Subject, nil)
//		if err != nil {
//			return err
//		}
//		if err := h.sessions.SetCookie(c, value); err != nil {
//			return err
//		}
//		// h.csrf is csrfPlugin.Get(ctx).Value from the login plugin's Build.
//		c.JSON(http.StatusOK, map[string]string{"csrf_token": h.csrf.TokenFor(value.ID)})
//		return nil
//	}
//
// New and DefaultConfig support direct construction by custom hosts:
//
//	cfg := csrf.DefaultConfig()
//	cfg.Mode = "token"
//	cfg.Secret = deploymentSecret
//	middleware, err := csrf.New(cfg)
//	if err != nil {
//		return err
//	}
//	router.Group("/", middleware.Handler())
//
// Bundle and ordinary imports are side-effect free. Applications that prefer
// xbc.Run may opt into process-global composition through this package's
// autoload leaf.
package csrf
