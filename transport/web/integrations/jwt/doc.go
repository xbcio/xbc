// Package jwt provides HMAC JWT authentication for the XBC Web transport.
//
// # Usage
//
// Configure plugins.jwt.secret with at least 32 bytes, enable the autoload
// package, and read only claims installed by the authentication middleware.
// ClaimsFromContext never returns unparsed bearer-token data:
//
//	func profile(c *gin.Context) {
//		claims, claimsOK := jwt.ClaimsFromContext(c)
//		principal, principalOK := web.CurrentPrincipal(c)
//		if !claimsOK || !principalOK {
//			c.AbortWithStatus(http.StatusUnauthorized)
//			return
//		}
//		role, _ := claims["role"].(string)
//		c.JSON(http.StatusOK, gin.H{
//			"subject": principal.Subject,
//			"role":    role,
//		})
//	}
//
// A directly assembled plugin can also issue bounded tokens. SignWithTTL owns
// exp, iat, and sub, so values with those names in the caller's map cannot
// override them:
//
//	func issueToken(auth *jwt.Plugin) (string, error) {
//		return auth.SignWithTTL(
//			"user:42",
//			jwt.Claims{"role": "admin"},
//			15*time.Minute,
//		)
//	}
//
// Verification is restricted to the configured HMAC SHA-2 allowlist. Do not
// use this symmetric-key plugin where independent asymmetric issuers are
// required.
//
// Importing this package has no registration side effects. Applications that
// use XBC's process-wide catalog opt in with a blank import of
// github.com/xbcio/xbc/transport/web/integrations/jwt/autoload; applications with a private
// catalog can use Definition directly.
package jwt
