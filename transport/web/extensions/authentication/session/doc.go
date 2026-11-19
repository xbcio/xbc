// Package session carries XBC's opaque server-side sessions over the Web
// transport.
//
// Session storage, lifecycle, verification, rotation, and attribute updates
// belong to the neutral session plugin at
// github.com/xbcio/xbc/extensions/authentication/session; this package moves
// the opaque ID between a request and a response and layers the login/logout
// helpers a Web application needs on top of that manager. Identity and
// attribute data remain server-side; a credential contains only the
// cryptographically random identifier. Bundle includes the neutral plugin, and
// Bundle and ordinary imports are side-effect free; xbc.Run applications may
// opt into the leaf autoload adapter.
//
// # Carriers
//
// A Carrier is one way a session ID travels: CookieCarrier installs and clears
// an HTTP cookie, while HeaderCarrier reads a request header and never writes.
// The default is the cookie carrier alone, using the cookie settings published
// by plugins.session. Selecting more than one carrier is optional and is done
// through plugins.session-http:
//
//	plugins:
//	  session:
//	    name: sid
//	  session-http:
//	    carriers: [cookie, header]
//	    header:
//	      name: Authorization
//	      scheme: Session
//
// The chain is ordered: the first Presented value wins, and any Malformed
// source fails the entire chain, so a duplicated cookie or header is never
// masked by a later usable value. Exactly one carrier may write, and the header
// carrier never does; a chain containing only the header carrier can verify
// existing sessions but reports ErrNoWritableCarrier when a login tries to
// install one. Presenting plugins.session-http never activates this plugin:
// activation remains tied to plugins.session, and configuring session-http
// without it leaves both plugins dormant.
//
// Direct construction accepts the same choices through New:
//
//	adapter, err := session.New(core,
//		session.WithConfig(session.Config{
//			Carriers: []string{session.CarrierCookie, session.CarrierHeader},
//		}))
//
// WithCarriers supplies an explicit ordered chain instead, for a carrier this
// package does not ship.
//
// # Usage
//
// The Definition exports the Web Manager contract -- the neutral Manager plus
// the two cookie methods, with no method of its own beyond that original set --
// and the Web CredentialExtractor. The request-scoped helpers documented by
// Handler (Login, Logout, Refresh, Patch) live on the adapter itself, so
// resolve the primary value when an application needs them:
//
//	var sessions = plugin.RefTo[*session.Plugin](session.Key)
//
//	type Login struct {
//		sessions *session.Plugin
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
//		r.POST("/logout", p.logout).Name("logout").Auth(web.Public())
//	}
//
//	func (p *Login) login(ctx context.Context, c *web.Ctx) error {
//		// Authenticate the submitted credentials before this point.
//		value, err := p.sessions.Login(ctx, c, "user:42", map[string]any{
//			"role": "admin",
//		})
//		if err != nil {
//			return err
//		}
//		c.JSON(http.StatusOK, map[string]any{"subject": value.Subject})
//		return nil
//	}
//
//	func (p *Login) logout(ctx context.Context, c *web.Ctx) error {
//		if err := p.sessions.Logout(ctx, c); err != nil {
//			return err
//		}
//		c.Status(http.StatusNoContent)
//		return nil
//	}
//
// Login revokes a session the request already presents before creating the new
// one, so a credential minted before authentication cannot survive login. It
// writes through the single writable carrier; when writing fails, the new
// session is revoked and no credential is left on the response. When the
// neutral manager refuses the session with ErrTooManySessions (max_sessions
// with reject_new), that error is returned unchanged and nothing is written.
// Logout is idempotent for absent, malformed, and already revoked sessions.
// Refresh replaces the current session's attributes in place, and Patch applies
// a set/unset delta; neither rotates the ID or extends the lifetime.
//
// Protected handlers read the authenticated session view through Current, or
// the explicit ID through CurrentID. Current returns a copy of the attributes
// with the session_id bookkeeping entry removed; the ID is a bearer credential
// and must never reach a log, an error message, or a response body.
//
// # Relationship to the Web authentication middleware
//
// The extractor plugs into the Web transport's built-in authentication
// middleware, which is the only place it runs. The middleware calls the
// neutral authenticator, which verifies the ID and publishes the session's
// attributes as authentication.Principal.Attributes, alongside the opaque ID
// under the "session_id" key. This package never writes a 401, a 503, or a
// problem response, and never adds a WWW-Authenticate challenge: a missing or
// dead session has no retryable HTTP authentication scheme, so the middleware's
// uniform rejection is the whole client-facing answer.
//
// # Comparison with Spring Session
//
//	Spring concept                    XBC equivalent
//	HttpSession / session id          neutral Manager.Create / Rotate
//	session.setAttribute              Refresh (whole map) or Patch (delta)
//	session.invalidate                Manager.Revoke / Logout
//	changeSessionId on login          Login revoking the presented session first
//	RequestContextHolder              web.CurrentPrincipal, web.Ctx
//	web.xml HttpSessionListener       neutral session Observer
//
// The anti-fixation step is explicit rather than automatic: Login revokes the
// valid session the request presents before it creates the replacement, which
// is the same outcome as migrateSession, but the decision and its timing stay
// visible in application code.
//
// # Mounted third-party HTTP subtrees
//
// A subtree mounted through an interop mount plugin is matched and served by
// its own engine's middleware stack, so the Web transport's authentication
// middleware never sees those requests and this extractor never runs for them.
// The host must verify the session in its own request path through the neutral
// plugin's AttributeManager.Verify before trusting it.
package session
