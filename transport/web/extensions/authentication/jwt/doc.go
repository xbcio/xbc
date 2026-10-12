// Package jwt is the Web credential extractor for XBC's HMAC JWT
// authentication.
//
// Verification, signing, and configuration live in the protocol-neutral
// github.com/xbcio/xbc/extensions/authentication/jwt. This package only reads
// the bearer token from the HTTP header that authenticator is configured with
// and hands it to the Web transport's built-in authentication middleware,
// which is the only place the live request is available. It has no
// configuration section of its own: configuring plugins.jwt activates both
// halves, and Bundle selects both.
//
// # Usage
//
// Configure plugins.jwt.secret with at least 32 bytes, select Bundle (or blank
// import the autoload package), and read the identity the middleware
// publishes. Verified claims travel as authentication.Principal.Attributes:
//
//	func profile(_ context.Context, c *web.Ctx) error {
//		principal, ok := web.CurrentPrincipal(c)
//		if !ok {
//			c.Status(http.StatusUnauthorized)
//			c.Abort()
//			return nil
//		}
//		role, _ := principal.Attributes["role"].(string)
//		c.JSON(http.StatusOK, map[string]any{
//			"subject": principal.Subject,
//			"role":    role,
//		})
//		return nil
//	}
//
// Issue tokens with the neutral plugin's Sign and SignWithTTL.
//
// Importing this package has no registration side effects. Applications that
// use XBC's process-wide catalog opt in with a blank import of
// github.com/xbcio/xbc/transport/web/extensions/authentication/jwt/autoload;
// applications with a private catalog can use Definition directly.
package jwt
