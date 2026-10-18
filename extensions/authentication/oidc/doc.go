// Package oidc implements the OpenID Connect authorization-code flow for XBC:
// discovery, JWKS management, the token exchange, and ID token verification.
// It is a protocol client only. It registers no routes, creates no sessions,
// and never decides which local user an external identity belongs to -- the
// verified result travels as federation.ExternalIdentity to the application's
// IdentityResolver.
//
// # Usage
//
// Construct a provider from static configuration, register it under its name,
// and discover its metadata. A web adapter that starts logins calls Begin and
// stores the returned transaction; the callback side passes the same
// transaction to Exchange:
//
//	provider, err := oidc.New(oidc.Config{
//		Name:         "corp",
//		Issuer:       "https://idp.example.com",
//		ClientID:     "xbc-admin",
//		ClientSecret: outbound.Secret(secret),
//		RedirectURI:  "https://admin.example.com/auth/corp/callback",
//		Scopes:       []string{"openid", "profile", "email"},
//	})
//	if err != nil {
//		return err
//	}
//	registry := oidc.NewRegistry()
//	if err := registry.Put("corp", provider); err != nil {
//		return err
//	}
//	if err := provider.Discover(ctx); err != nil {
//		return err
//	}
//	transaction, err := provider.Begin()
//	if err != nil {
//		return err
//	}
//	// Store state, nonce, and code verifier; then, on the callback:
//	identity, err := provider.Exchange(ctx, code, transaction)
//
// # Security
//
// ID token signatures are verified against an asymmetric algorithm allowlist.
// alg=none and the HMAC family are rejected at configuration time, so a JWKS
// public key can never be confused with an HMAC secret. The issuer must match
// the registered value exactly, the audience must contain this client, azp is
// enforced when aud carries several values, exp, nbf, and iat are checked
// against an injectable clock with a leeway capped at 60 seconds, nonce must
// echo the transaction, and at_hash must match the access token when present.
//
// state and PKCE (S256) are mandatory: Begin always produces both, and
// Exchange refuses a transaction without a nonce and a well-formed verifier.
// The redirect URI is the configured value, sent verbatim; wildcards are
// rejected at construction and plain HTTP requires the AllowHTTP development
// switch. Only the authorization-code flow is implemented -- implicit and
// hybrid responses are not supported.
//
// The discovery document and the JWKS are always fetched from URLs derived
// from the registered issuer, never from a request parameter. A discovery
// document whose issuer does not match the registered value is rejected. The
// JWKS is cached with a TTL, keys are selected by key ID, and an unknown key
// ID triggers at most one refetch per refresh interval, so token verification
// cannot be used as a request amplifier. Old and new keys coexist for as long
// as the IdP publishes both.
//
// Every outbound request uses the federation/outbound client: HTTPS, address
// policy, per-hop redirect validation, and timeout and body-size limits apply
// to discovery, the JWKS, and the token endpoint alike. Client credentials
// travel in the Authorization header or the request body, never in a URL, and
// no error text or log view carries a client secret, a token, or an IdP
// response body.
//
// # Lifecycle
//
// A Provider starts no goroutines, owns no background refresher, and performs
// no asynchronous work: discovery and JWKS refreshes happen inline in the call
// that needs them. The package therefore implements neither plugin.Drainer nor
// Stop. The HTTP client keeps idle connections only until the outbound
// transport's idle timeout reclaims them, so retiring a provider needs no
// shutdown call. Registry.Remove affects only subsequent logins -- sessions
// created earlier stay valid until the application revokes them.
package oidc
