// Package apikey provides API-key authentication for XBC's Web transport.
// Presented keys are hashed immediately and repositories only receive SHA-256
// digests; the built-in repository never retains plaintext.
//
// # Usage
//
// Compose Bundle explicitly to use configuration-backed static credentials.
// Bundle and ordinary package imports are side-effect free. Executables that
// intentionally use XBC's optional process-wide composition can import the leaf
// apikey/autoload package instead.
//
// For direct construction, Config.Static builds the same digest-only repository
// used by the canonical Definition. WithRepository selects an external store
// instead and is mutually exclusive with Config.Static:
//
//	func newAPIKeyPlugin(plaintext string) (*apikey.Plugin, error) {
//		repository, err := apikey.NewStaticRepository([]apikey.StaticCredential{{
//			ID:      "payments-v1",
//			AppID:   "checkout",
//			Subject: "service:payments",
//			SHA256:  apikey.HashKey(plaintext).String(),
//			Attributes: map[string]any{
//				"role": "writer",
//			},
//		}})
//		if err != nil {
//			return nil, err
//		}
//		return apikey.New(apikey.DefaultConfig(), apikey.WithRepository(repository))
//	}
//
// Public routes must opt out explicitly with Route.Auth(web.Public()). On a
// successfully authenticated protected request, consume the shared Web
// principal rather than reading the credential header again:
//
//	principal, ok := web.CurrentPrincipal(c)
//	if !ok || principal.AuthMethod != "apikey" {
//		c.AbortWithStatus(http.StatusUnauthorized)
//		return
//	}
//	c.JSON(http.StatusOK, gin.H{"subject": principal.Subject})
//
// Production repositories implement Repository and receive only KeyDigest.
// Implementations must honor context cancellation and compare secret material
// in constant time.
package apikey
