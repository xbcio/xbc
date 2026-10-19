// Package saml is the Web adapter for the neutral SAML 2.0 service provider.
// It owns the public start, assertion consumer service (ACS), and metadata
// routes, drives one SP-initiated login through the shared federation login
// base, and hands the verified external identity to the application's
// IdentityResolver. It decides nothing about protocol validation -- the neutral
// service provider owns XML parsing, signature verification, condition checks,
// and replay detection -- and nothing about which local user an external
// identity belongs to.
//
// # Routes
//
// The adapter contributes three explicitly public routes; a browser reaching
// the login routes has no session yet by definition, and an IdP administrator
// fetching metadata cannot hold one either.
//
//	GET  {prefix}/{provider}/start?return_to=...
//	POST {prefix}/{provider}/acs
//	GET  {prefix}/{provider}/metadata
//
// prefix defaults to /auth/saml and is configurable at plugins.saml-http.prefix.
// The configured ACS URL of every registered provider must name the same
// {prefix}/{provider}/acs path, because both the AuthnRequest and the SP
// metadata publish it. Each protocol adapter mounts the same start route shape,
// so two adapters must use different prefixes; sharing one would register the
// same routes twice.
//
// The provider path parameter is validated against
// ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$ before the registry is consulted. On start
// and ACS an unregistered provider and a malformed one produce the same fixed
// failure code; on metadata they produce the same empty 404. The submitted
// value is never echoed.
//
// # Login flow
//
// Start asks the neutral service provider for a fresh AuthnRequest: a 128-bit
// request ID, the deflated and base64-encoded HTTP-Redirect binding, and, when
// an SP private key is configured, the RSA-SHA256 signature. It generates a
// 128-bit RelayState and stores the transaction through the shared base --
// provider, state, request ID, and return_to -- bound to the initiating
// browser, namespaced by provider, single-use, and short-lived. The binding
// cookie is installed only after the transaction is durable, and only then is
// the browser redirected to the IdP with Cache-Control: no-store.
//
// The IdP posts the response to the ACS. The adapter reads the form under a
// bounded size, requires exactly one SAMLResponse and one RelayState, and
// consumes the transaction through the shared base before decoding anything.
// A replay, an expired transaction, a response from another browser, and a
// malformed body therefore all burn the transaction they name. The decoded
// response is bounded again and passed to the neutral service provider with
// the transaction's request ID and the provider's configured ACS URL -- the
// trust anchor, never the request Host. The neutral module verifies the single
// signature over the element it names, the issuer, status, destination,
// audience, recipient, InResponseTo, validity window, NameID format, and the
// assertion replay cache. Only then does the shared base resolve the identity,
// revoke any session the request presented, create a new session, install its
// cookie, and redirect to the sanitized return_to.
//
// SAML cross-site POST: an IdP posts to the ACS from its own origin, so the
// browser-binding cookie is only sent when its SameSite policy permits a
// cross-site POST. Deployments with a cross-origin IdP must configure
// plugins.federation-http.cookie_same_site: none (which requires
// cookie_secure: true).
//
// # Replay protection
//
// The neutral service provider claims every accepted assertion ID in a
// saml.ReplayCache. The replay cache must be shared by every replica that can
// receive a response for the same provider, or two replicas will each accept
// the same assertion once. The application injects NewReplayCache at provider
// construction:
//
//	cache, err := samlweb.NewReplayCache(login, "corp")
//	if err != nil {
//		return err
//	}
//	provider, err := saml.New(saml.Config{
//		Name:        "corp",
//		// ...
//		ReplayCache: cache,
//	})
//
// NewReplayCache delegates to the shared base's atomic set-if-absent
// reservation, so it inherits the configured backend: Redis (the default)
// shares reservations across replicas, while the memory backend protects only
// the process that served the first response. Assertion IDs are namespaced by
// provider. A cache failure fails the login closed; an assertion whose validity
// window exceeds the store's 24-hour reservation bound is rejected rather than
// cached for a shorter time.
//
// # Failure reporting
//
// Every login failure redirects to the shared base's configured failure landing
// with one closed code: federation_denied (the application refused the
// verified identity), federation_failed (an operational failure, including an
// unavailable replay cache), or federation_bad_request (malformed request
// material, an invalid or replayed assertion). No IdP text, RelayState,
// assertion, NameID, certificate, or key reaches the browser. Detailed reasons
// go to the internal log only; the neutral module guarantees its error values
// carry no assertion content.
//
// # Assembly
//
// The Definition requires exactly one shared federation login base
// (federation.Login, exported by the federation Web base) and exactly one SAML
// provider registry (the value saml.NewRegistry returns). A missing or
// duplicated implementation fails at plan time with a diagnostic naming the
// contract. The adapter's Bundle selects the shared base and the session
// plugins it requires, so a composition only needs to configure
// plugins.session, plugins.federation-http, and plugins.saml-http. The
// application still provides the IdentityResolver the base requires and owns
// the registry: it loads providers (explicitly fetching IdP metadata when it
// chooses) and exports the registry as its plugin's primary value.
//
// # Usage
//
//	provider, err := saml.New(saml.Config{
//		Name:            "corp",
//		Issuer:          "https://idp.example.com/entity",
//		SSOURL:          "https://idp.example.com/sso",
//		EntityID:        "https://admin.example.com/saml/entity",
//		ACSURL:          "https://admin.example.com/auth/saml/corp/acs",
//		IdPCertificates: []*x509.Certificate{certificate},
//		ClaimAttributes: map[string]string{"email": "mail"},
//		ReplayCache:     cache,
//	})
//	if err != nil {
//		return err
//	}
//	registry := saml.NewRegistry()
//	if err := registry.Put("corp", provider); err != nil {
//		return err
//	}
//	// Export registry from an application plugin as its primary value; select
//	// samlweb.Bundle() and configure plugins.saml-http,
//	// plugins.federation-http, and plugins.session.
//
// # Lifecycle
//
// The adapter starts no goroutine, owns no connection, and performs no
// asynchronous work: everything happens inline in the request that needs it.
// The package therefore implements neither plugin.Drainer nor Stop. The
// registry and every provider's replay cache are owned by the application, and
// removing a provider affects only subsequent logins -- sessions created
// earlier stay valid until the application revokes them.
package saml
