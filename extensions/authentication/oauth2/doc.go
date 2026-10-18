// Package oauth2 implements the OAuth 2.0 authorization-code flow for XBC and
// turns the provider's userinfo response into a federation.ExternalIdentity.
// It is a protocol client only. It registers no routes, creates no sessions,
// and never decides which local user an external identity belongs to -- the
// verified result travels to the application's IdentityResolver.
//
// # What OAuth 2.0 proves
//
// OAuth 2.0 alone does not prove an identity: it proves that the browser
// authorized this client and that the client received a token. The identity is
// whatever the provider's userinfo endpoint reports for that token, so every
// provider must configure the userinfo endpoint and the fields that carry the
// account identity. The subject is read from a configured dotted path and must
// be a non-empty JSON string. It must name the provider's stable account
// identifier -- never an email address, a login name, or another mutable and
// reassignable attribute. There is no fallback: a missing subject fails the
// login.
//
// # Usage
//
// Construct a provider from static configuration and register it under its
// name. There is no discovery step; every endpoint is configured. A web
// adapter that starts logins calls Begin and stores the returned transaction;
// the callback side passes the same transaction to Exchange:
//
//	provider, err := oauth2.New(oauth2.Config{
//		Name:                  "legacy",
//		Issuer:                "https://legacy-idp.example.com",
//		AuthorizationEndpoint: "https://legacy-idp.example.com/oauth/authorize",
//		TokenEndpoint:         "https://legacy-idp.example.com/oauth/token",
//		UserInfoEndpoint:      "https://legacy-idp.example.com/api/account",
//		ClientID:              "xbc-admin",
//		ClientSecret:          outbound.Secret(secret),
//		RedirectURI:           "https://admin.example.com/auth/oauth2/legacy/callback",
//		SubjectField:          "account.id",
//		ClaimFields:           map[string]string{"email": "email", "name": "account.display_name"},
//	})
//	if err != nil {
//		return err
//	}
//	registry := oauth2.NewRegistry()
//	if err := registry.Put("legacy", provider); err != nil {
//		return err
//	}
//	transaction, err := provider.Begin()
//	if err != nil {
//		return err
//	}
//	// Store state and the PKCE verifier; then, on the callback:
//	identity, err := provider.Exchange(ctx, code, transaction)
//
// # Security
//
// state and PKCE (S256) are mandatory in the flow: Begin always produces both,
// and Exchange refuses a transaction without a well-formed verifier. Matching
// state to the stored transaction and consuming both exactly once is the web
// adapter's job, delegated to the shared federation login base.
//
// The token endpoint requires an explicitly configured client authentication
// method (client_secret_basic, the default, or client_secret_post).
// Credentials travel only in the Authorization header or the request body,
// never in a URL. The token response must carry token_type Bearer and an
// access token. The access token is used for exactly one userinfo call,
// travels only in that request's Authorization header, and is never stored,
// logged, or returned; it is not a refreshable credential and no refresh token
// is read or kept.
//
// The userinfo request goes to the registered endpoint verbatim and uses a
// client that rejects every redirect, so a response can never be served from a
// location the application did not register. Every outbound request uses the
// federation/outbound client: HTTPS, address policy, per-hop redirect
// validation, and timeout and body-size limits apply to the token and userinfo
// endpoints alike. A non-success status is reported by its status code alone;
// the provider's response body is never echoed into an error. No error text or
// log view carries a client secret, a token, or a response body.
//
// OAuth 2.0 carries no signed timestamp, so there is no exp/nbf window and no
// leeway to apply. The injected clock stamps ExternalIdentity.AuthTime, which
// becomes the auth_time session attribute.
//
// # Lifecycle
//
// A Provider starts no goroutines and performs no asynchronous work: the token
// exchange and the userinfo call happen inline in Exchange. The package
// therefore implements neither plugin.Drainer nor Stop. The HTTP clients keep
// idle connections only until the outbound transport's idle timeout reclaims
// them, so retiring a provider needs no shutdown call. Registry.Remove affects
// only subsequent logins -- sessions created earlier stay valid until the
// application revokes them.
package oauth2
