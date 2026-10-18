package oauth2

import "errors"

var (
	// ErrInvalidConfig reports a configuration that violates a static rule.
	// New and ValidateProvider reject it before any network call, and a
	// rejected registry Put leaves the previous configuration in effect.
	ErrInvalidConfig = errors.New("oauth2: invalid configuration")
	// ErrNilContext reports a nil context passed to Exchange.
	ErrNilContext = errors.New("oauth2: nil context")
	// ErrNilProvider reports a method call on a nil *Provider.
	ErrNilProvider = errors.New("oauth2: nil provider")
	// ErrInvalidTransaction reports login transaction material that cannot be
	// exchanged: a malformed PKCE verifier or a missing or oversized
	// authorization code.
	ErrInvalidTransaction = errors.New("oauth2: invalid login transaction")
	// ErrTokenExchange reports a token endpoint call that failed at the
	// transport level or returned a non-success status.
	ErrTokenExchange = errors.New("oauth2: token exchange failed")
	// ErrInvalidTokenResponse reports a token response that is not a Bearer
	// token response carrying an access token.
	ErrInvalidTokenResponse = errors.New("oauth2: invalid token response")
	// ErrUserInfo reports a userinfo call that failed at the transport level,
	// returned a non-success status, or exceeded the response body limit.
	ErrUserInfo = errors.New("oauth2: userinfo request failed")
	// ErrInvalidUserInfo reports a userinfo response that is not a JSON object
	// or does not carry every configured field.
	ErrInvalidUserInfo = errors.New("oauth2: invalid userinfo response")
)
