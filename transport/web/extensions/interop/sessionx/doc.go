// Package sessionx binds the neutral session core to a Gin host.
//
// The neutral session package owns what a session is: creation, rotation,
// revocation, verification, idle renewal, persistence, and the cookie
// configuration. It is protocol-neutral. A transport only moves the opaque
// session ID between a request and that core. The Web adapter does this for the
// Web transport's own request type; this package does it for *gin.Context. Both
// use the same core and the same cookie, so a Gin application and the rest of
// the service can share one session store without adopting the Web transport.
//
// # Usage
//
// Build the neutral session plugin, then hand its manager, cookie settings, and
// lifetime to New:
//
//	core, err := session.New(session.DefaultConfig())
//	if err != nil {
//		return err
//	}
//	defer core.Stop(context.Background())
//
//	handler, err := sessionx.New(core, core.Cookie(), core.Lifetime)
//	if err != nil {
//		return err
//	}
//
//	router := gin.New()
//	router.Use(handler.Middleware())
//	router.GET("/me", func(c *gin.Context) {
//		value, ok := handler.Current(c)
//		if !ok {
//			// The host owns the authentication policy: 401, 500, or a
//			// documented public response.
//			c.Status(http.StatusUnauthorized)
//			return
//		}
//		c.JSON(http.StatusOK, gin.H{"subject": value.Subject})
//	})
//
// # The middleware decides nothing
//
// Middleware verifies the cookie and publishes the outcome on the Gin context.
// It never aborts the chain and never writes a response: which routes require a
// session and what a rejection looks like belong to the host, whose public
// routes and error vocabulary this package cannot know. The outcome is one of:
// no cookie was presented, a valid session, a presented but unusable session
// (duplicate, empty, malformed, expired, revoked, or unknown), or a storage
// failure. The neutral manager deliberately does not distinguish an expired
// session from one that never existed, so neither does this package; a host
// that needs to tell a storage failure from an unusable credential reads
// StoreError.
//
// Login and Logout are explicit calls, not middleware. A login endpoint calls
// Login after it has authenticated the subject; a logout endpoint calls Logout.
// Login revokes a valid session the request already carries before creating its
// replacement, so a session ID fixed before login cannot survive it.
//
// # Context keys
//
// The outcome is stored under DefaultContextKey ("xbc.session") unless
// WithContextKey selects another key. The Handler methods follow the Handler's
// key, and the package-level helpers read DefaultContextKey. Two Handlers
// configured with different keys can therefore run in one request without
// reading each other's outcome.
//
// The opaque session ID is a bearer credential. Neither the errors nor the
// implementation of this package ever report it; do not log a Session.ID.
package sessionx
