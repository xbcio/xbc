// Package jwt provides protocol-neutral HMAC JWT authentication for XBC.
//
// It owns everything about a token except where a request carries it:
// configuration, signing, signature and claim verification, and the
// authentication.Authenticator for the "jwt" scheme. A transport adapter
// extracts the raw token -- the Web adapter at
// github.com/xbcio/xbc/transport/web/extensions/authentication/jwt reads it
// from the configured HTTP header -- and the transport's authentication
// manager hands it to Authenticate. Header and TokenScheme publish the
// configured carriage so every adapter extracts from the same place.
//
// # Usage
//
// Configure plugins.jwt.secret with at least 32 bytes and select a transport
// adapter's Bundle, which includes this one. A directly constructed plugin can
// also issue bounded tokens. SignWithTTL owns exp, iat, and sub, so values with
// those names in the caller's map cannot override them:
//
//	func issueToken(auth *jwt.Plugin) (string, error) {
//		return auth.SignWithTTL(
//			"user:42",
//			jwt.Claims{"role": "admin"},
//			15*time.Minute,
//		)
//	}
//
// Verified claims travel as authentication.Principal.Attributes. Verification
// is restricted to the configured HMAC SHA-2 allowlist. Do not use this
// symmetric-key plugin where independent asymmetric issuers are required.
//
// Importing this package has no registration side effects. Applications that
// use XBC's process-wide catalog opt in with a blank import of
// github.com/xbcio/xbc/extensions/authentication/jwt/autoload; applications
// with a private catalog can use Definition directly.
package jwt
