// Package session carries XBC's opaque server-side sessions in HTTP cookies.
//
// Session storage, lifecycle, and verification belong to the neutral session
// plugin at github.com/xbcio/xbc/extensions/authentication/session; this
// package only reads the session ID from the configured cookie and installs or
// clears that cookie. Cookies contain only the cryptographically random
// identifier, and all identity and attribute data remains server-side. Bundle
// includes the neutral plugin, and Bundle and ordinary imports are side-effect
// free; xbc.Run applications may opt into the leaf autoload adapter.
//
// The extractor plugs into the Web transport's built-in authentication
// middleware, which is the only place a credential extractor runs. The
// session's stored attributes travel as authentication.Principal.Attributes,
// alongside its opaque ID under the "session_id" key:
//
//	func profile(ctx context.Context, c *web.Ctx) error {
//		principal, ok := web.CurrentPrincipal(c)
//		if !ok {
//			web.AbortProblem(c, web.NewProblem(http.StatusUnauthorized, "unauthorized"))
//			return nil
//		}
//		role, _ := principal.Attributes["role"].(string)
//		sessionID, _ := principal.Attributes["session_id"].(string)
//		c.JSON(http.StatusOK, map[string]any{
//			"subject":    principal.Subject,
//			"role":       role,
//			"session_id": sessionID,
//		})
//		return nil
//	}
//
// # Usage
//
// Application plugins can require this package's Manager -- the neutral
// session lifecycle plus SetCookie and ClearCookie -- create a session after
// verifying login credentials, and install its opaque cookie. Mark the login
// route public so session authentication does not require a cookie first:
//
//	var sessions = plugin.RefTo[session.Manager](session.Key)
//
//	type Login struct {
//		sessions session.Manager
//	}
//
//	var loginDefinition = plugin.Define(
//		"login",
//		func(ctx plugin.BuildContext) (*Login, error) {
//			return &Login{sessions: sessions.Get(ctx).Value}, nil
//		},
//		plugin.Options[*Login]{Inputs: plugin.Inputs(sessions)},
//	)
//
//	func (p *Login) RegisterRoutes(r *web.Router) {
//		// Auth(web.Public()) is a tier-2 declaration: the route is public unless
//		// a tier-1 web.security policy rule overrides it.
//		r.POST("/login", p.login).Name("login").Auth(web.Public())
//	}
//
//	func (p *Login) login(ctx context.Context, c *web.Ctx) error {
//		// Authenticate the submitted credentials before this point.
//		value, err := p.sessions.Create(ctx, "user:42", map[string]any{
//			"role": "admin",
//		})
//		if err == nil {
//			err = p.sessions.SetCookie(c, value)
//		}
//		if err != nil {
//			return err
//		}
//		c.Status(http.StatusNoContent)
//		return nil
//	}
//
// Protected handlers read the authenticated session's attributes and ID from
// web.CurrentPrincipal, as shown above. Rotate the session after privilege
// changes, revoke it on logout, and retain the secure, HTTP-only cookie
// defaults in production. Use the Redis backend when sessions must be shared
// across application replicas.
package session
