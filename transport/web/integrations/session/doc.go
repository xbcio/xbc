// Package session provides opaque, server-side HTTP cookie sessions for XBC.
//
// Cookies contain only a cryptographically random identifier. Built-in stores
// hash that identifier before using it as a storage key, and all identity and
// attribute data remains server-side. Bundle and ordinary imports are
// side-effect free; xbc.Run applications may opt into the leaf autoload
// adapter.
//
// # Usage
//
// Application plugins can require the provided Manager, create a session after
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
//		r.POST("/login", p.login).Name("login").Auth(web.Public())
//	}
//
//	func (p *Login) login(c *gin.Context) {
//		// Authenticate the submitted credentials before this point.
//		value, err := p.sessions.Create(c.Request.Context(), "user:42", map[string]any{
//			"role": "admin",
//		})
//		if err == nil {
//			err = p.sessions.SetCookie(c, value)
//		}
//		if err != nil {
//			c.AbortWithStatus(http.StatusInternalServerError)
//			return
//		}
//		c.Status(http.StatusNoContent)
//	}
//
// Protected handlers can call Current for the authenticated server-side value.
// Rotate the session after privilege changes, revoke it on logout, and retain
// the secure, HTTP-only cookie defaults in production. Use the Redis backend
// when sessions must be shared across application replicas.
package session
