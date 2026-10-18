// Package federation is the shared web login base for XBC's federation
// adapters: the OIDC, OAuth2, and SAML Web adapters consume it instead of each
// carrying its own transaction store, return_to policy, and session handoff.
// The package itself knows nothing about any protocol -- it never imports a
// protocol module -- and it never decides which local user an external
// identity belongs to.
//
// # Login transactions
//
// A login starts with Begin. The adapter hands it the one-time material its
// protocol module generated (state, and as applicable a nonce, a PKCE
// verifier, or a SAML request ID) plus the browser's requested return_to.
// Begin sanitizes the landing location, generates a browser-binding secret,
// stores the transaction, and installs that secret as an HttpOnly cookie.
//
// Consume destroys the transaction and returns it only to the browser that
// started the login. The store implementations make deletion and lookup one
// atomic operation, so two concurrent callbacks cannot both succeed, and the
// binding digest is compared in constant time. A missing transaction, an
// expired one, a replay, and a callback from a different browser are all
// reported as found == false; callers must not distinguish them outward.
//
// A transaction lives at most MaxTransactionTTL (ten minutes) and is never
// extended. Transaction keys are namespaced by provider.
//
// The RedisStore backend is the default: it shares transactions through the
// Redis client instance named by plugins.federation-http, so an authorization
// request served by one replica and a callback served by another find the same
// transaction. The MemoryStore backend keeps transactions in this process and
// is an explicit single-replica choice only.
//
// # return_to
//
// By default a login lands only on a same-origin relative path: exactly one
// leading slash, no backslash, no control characters, and no percent-encoded
// delimiter a downstream decoder could turn into one. An absolute URL is
// accepted only when its https origin appears in the explicit
// allowed_return_origins allowlist. An unacceptable value silently becomes the
// configured default; the browser is never told why.
//
// # Session handoff
//
// Complete runs the whole handoff for an already verified external identity:
// it calls the application's IdentityResolver through the federation
// contract, merges the reserved session attributes, revokes any session the
// request presented, creates a new session, installs its cookie, and redirects
// to the sanitized landing location.
//
// The presented session is revoked before the new one is created. That is what
// prevents session fixation: an ID the browser held before the login never
// survives it. The new session is created rather than rotated, because
// rotation preserves the previous absolute lifetime and a login starts a fresh
// one. Session validity is independent of any IdP token: tokens are never
// stored in the session.
//
// Every failure redirects to the configured failure landing with one fixed
// code -- CodeDenied, CodeFailed, or CodeBadRequest. No IdP text, state value,
// assertion, or exception text reaches the browser; the detailed reason goes
// to the internal log only, and transaction material is never logged.
//
// The session subject is the local user ID returned by the application, never
// the IdP subject or an email address. The protocol name travels as the
// reserved idp_protocol session attribute, so Principal.AuthMethod remains the
// session authenticator's own "session".
//
// # Assembly
//
// The Definition requires exactly one federation.IdentityResolver, exactly one
// Web session adapter, and the neutral session plugin whose configuration owns
// the session cookie name. A missing or duplicated implementation fails at
// plan time with a diagnostic naming the contract. Construction rejects a
// binding cookie name equal to the session cookie name. The package starts no
// goroutine and owns no connection; the Redis client it borrows belongs to the
// plugin instance that supplied it, so the package implements neither
// plugin.Drainer nor Stop.
//
// # Usage
//
// An adapter package declares an exact reference to the shared base, binds it
// once in its own Definition, and calls it from its own public routes:
//
//	var loginInput = plugin.RefTo[federationweb.Login](federationweb.Key)
//
//	type Adapter struct {
//		login    federationweb.Login
//		provider *oidc.Provider
//	}
//
//	var adapterDefinition = plugin.Define(
//		"oidc-http",
//		func(ctx plugin.BuildContext) (*Adapter, error) {
//			return &Adapter{login: loginInput.Get(ctx).Value, provider: newProvider()}, nil
//		},
//		plugin.Options[*Adapter]{Inputs: plugin.Inputs(loginInput)},
//	)
//
//	func (p *Adapter) start(ctx context.Context, c *web.Ctx) error {
//		request, err := p.provider.Begin()
//		if err != nil {
//			p.login.Fail(c, federationweb.CodeBadRequest)
//			return nil
//		}
//		_, err = p.login.Begin(ctx, c, federationweb.BeginRequest{
//			Provider:     "corp",
//			State:        request.State,
//			Nonce:        request.Nonce,
//			CodeVerifier: request.CodeVerifier,
//			ReturnTo:     c.Query("return_to"),
//		})
//		if err != nil {
//			p.login.Fail(c, federationweb.CodeFailed)
//			return nil
//		}
//		c.SetHeader("Location", request.URL)
//		c.Status(http.StatusFound)
//		return nil
//	}
//
//	func (p *Adapter) callback(ctx context.Context, c *web.Ctx) error {
//		transaction, found, err := p.login.Consume(ctx, c, "corp", c.Query("state"))
//		if err != nil {
//			p.login.Fail(c, federationweb.CodeFailed)
//			return nil
//		}
//		if !found {
//			p.login.Fail(c, federationweb.CodeBadRequest)
//			return nil
//		}
//		identity, err := p.provider.Exchange(ctx, c.Query("code"), transaction)
//		if err != nil {
//			p.login.Fail(c, federationweb.CodeFailed)
//			return nil
//		}
//		return p.login.Complete(ctx, c, identity, transaction.ReturnTo)
//	}
//
// The shared base registers no route: each adapter owns its own start and
// callback routes and marks them public with web.Public().
package federation
