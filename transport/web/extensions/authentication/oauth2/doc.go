// Package oauth2 is the Web adapter for the neutral OAuth 2.0 client. It owns
// two public routes, drives one authorization-code login through the shared
// federation login base, and hands the verified external identity to the
// application's IdentityResolver. It decides nothing about protocol validation
// -- the neutral client owns the token exchange and the userinfo read -- and
// nothing about which local user an external identity belongs to.
//
// # Routes
//
// The adapter contributes two explicitly public routes; a browser reaching
// either has no session yet by definition.
//
//	GET {prefix}/{provider}/start?return_to=...
//	GET {prefix}/{provider}/callback
//
// prefix defaults to /auth/oauth2 and is configurable at plugins.oauth2-http.prefix.
// The configured redirect URI of every registered provider must carry the same
// path, because the neutral client sends that URI verbatim to the IdP. Each
// protocol adapter mounts the identical route shape, so two adapters must use
// different prefixes; sharing one would register the same routes twice.
//
// The provider path parameter is validated against
// ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$ before the registry is consulted. An
// unregistered provider and a malformed one produce the same fixed failure
// code, and the submitted value is never echoed.
//
// # Login flow
//
// Start asks the neutral client for a fresh transaction (state and a PKCE S256
// verifier) and its authorization URL, then stores the transaction through the
// shared base: bound to the initiating browser, namespaced by provider,
// single-use, and short-lived. The binding cookie is installed only after the
// transaction is durable, and only then is the browser redirected to the IdP
// with Cache-Control: no-store.
//
// The callback consumes the transaction through the shared base first, so a
// replay, an expired transaction, and a callback from a different browser all
// fail before any token work happens. The IdP's error response is handled
// after consumption as well, which burns the transaction it belongs to. Only
// then does the neutral client redeem the authorization code, present the PKCE
// verifier, read the account document, and return a verified
// federation.ExternalIdentity. The shared base resolves that identity, revokes
// any session the request presented, creates a new session, installs its
// cookie, and redirects to the sanitized return_to.
//
// OAuth 2.0 defines no ID token, so this adapter cannot verify an authentication
// assertion the way an OpenID Connect adapter does; the identity is whatever
// the configured userinfo endpoint returns for the issued access token. Bind
// providers to IdPs whose token and userinfo endpoints you trust.
//
// # Failure reporting
//
// Every failure redirects to the shared base's configured failure landing with
// one closed code: federation_denied (the application refused the identity, or
// the IdP reported access_denied), federation_failed (an operational failure,
// including a failed token exchange or userinfo read), or
// federation_bad_request (malformed or unusable request material). No IdP
// text, state value, or authorization code reaches the browser. Detailed
// reasons go to the internal log only; the provider name is logged after
// validation, and the IdP's error query parameter is logged only when it
// matches a conservative OAuth error-code shape.
//
// # Assembly
//
// The Definition requires exactly one shared federation login base
// (federation.Login, exported by the federation Web base) and exactly one
// OAuth 2.0 provider registry (the value oauth2.NewRegistry returns). A missing
// or duplicated implementation fails at plan time with a diagnostic naming the
// contract. The adapter's Bundle selects the shared base and the session
// plugins it requires, so a composition only needs to configure
// plugins.session, plugins.federation-http, and plugins.oauth2-http. The
// application still provides the IdentityResolver the base requires and owns
// the registry: it registers providers and exports the registry as its
// plugin's primary value.
//
// # Usage
//
//	provider, err := oauth2.New(oauth2.Config{
//		Name:                  "corp",
//		Issuer:                "https://idp.example.com",
//		AuthorizationEndpoint: "https://idp.example.com/oauth2/authorize",
//		TokenEndpoint:         "https://idp.example.com/oauth2/token",
//		UserInfoEndpoint:      "https://idp.example.com/oauth2/userinfo",
//		ClientID:              "xbc-admin",
//		ClientSecret:          outbound.Secret(secret),
//		RedirectURI:           "https://admin.example.com/auth/oauth2/corp/callback",
//	})
//	if err != nil {
//		return err
//	}
//	registry := oauth2.NewRegistry()
//	if err := registry.Put("corp", provider); err != nil {
//		return err
//	}
//	// Export registry from an application plugin as its primary value; select
//	// oauth2web.Bundle() and configure plugins.oauth2-http,
//	// plugins.federation-http, and plugins.session.
//
// # Lifecycle
//
// The adapter starts no goroutine, owns no connection, and performs no
// asynchronous work: everything happens inline in the request that needs it.
// The package therefore implements neither plugin.Drainer nor Stop. The
// registry is owned by the application, and removing a provider affects only
// subsequent logins -- sessions created earlier stay valid until the
// application revokes them.
package oauth2
