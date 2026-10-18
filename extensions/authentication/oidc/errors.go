package oidc

import "errors"

var (
	// ErrInvalidConfig reports a configuration that violates a static rule.
	// New and ValidateProvider reject it before any network call, and a
	// rejected registry Put leaves the previous configuration in effect.
	ErrInvalidConfig = errors.New("oidc: invalid configuration")
	// ErrNilContext reports a nil context passed to Discover or Exchange.
	ErrNilContext = errors.New("oidc: nil context")
	// ErrNilProvider reports a method call on a nil *Provider.
	ErrNilProvider = errors.New("oidc: nil provider")
	// ErrNotDiscovered reports that an operation needs provider metadata
	// before Discover has completed successfully.
	ErrNotDiscovered = errors.New("oidc: provider metadata is not discovered")
	// ErrDiscovery reports a discovery document that could not be fetched or
	// failed validation.
	ErrDiscovery = errors.New("oidc: discovery failed")
	// ErrInvalidTransaction reports login transaction material that cannot be
	// exchanged: a missing nonce, a malformed PKCE verifier, or a missing
	// authorization code.
	ErrInvalidTransaction = errors.New("oidc: invalid login transaction")
	// ErrTokenExchange reports a token endpoint call that failed at the
	// transport level or returned a non-success status.
	ErrTokenExchange = errors.New("oidc: token exchange failed")
	// ErrInvalidTokenResponse reports a token response that is not a Bearer
	// token response carrying both an ID token and an access token.
	ErrInvalidTokenResponse = errors.New("oidc: invalid token response")
	// ErrInvalidIDToken reports an ID token whose signature or claims failed
	// validation.
	ErrInvalidIDToken = errors.New("oidc: invalid ID token")
	// ErrUnknownKey reports that the JWKS holds no usable key for the ID
	// token's key ID. Repeated unknown key IDs are answered from the cached
	// key set until the refresh interval has passed.
	ErrUnknownKey = errors.New("oidc: no JWKS key matches the ID token key ID")
	// ErrJWKS reports a JWKS that could not be fetched or parsed.
	ErrJWKS = errors.New("oidc: JWKS retrieval failed")
)
