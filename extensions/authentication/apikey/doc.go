// Package apikey provides protocol-neutral API-key authentication for XBC.
// Presented keys are hashed immediately and repositories only receive SHA-256
// digests; the built-in repository never retains plaintext.
//
// It owns everything about a key except where a request carries it:
// configuration, the digest repository, and the authentication.Authenticator
// for the "apikey" scheme. A transport adapter extracts a PresentedKey -- the
// Web adapter at
// github.com/xbcio/xbc/transport/web/extensions/authentication/apikey reads
// it from the configured HTTP headers -- and the transport's authentication
// manager hands it to Authenticate. Header, AppIDHeader, AllowBearer, and
// BearerScheme publish the configured carriage so every adapter extracts from
// the same place.
//
// # Usage
//
// Configure plugins.apikey.static with digest-only credentials and select a
// transport adapter's Bundle, which includes this one. For direct
// construction, Config.Static builds the same digest-only repository used by
// the canonical Definition. WithRepository selects an external store instead
// and is mutually exclusive with Config.Static:
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
// Production repositories implement Repository and receive only KeyDigest.
// Implementations must honor context cancellation and compare secret material
// in constant time.
//
// Importing this package has no registration side effects. Applications that
// use XBC's process-wide catalog opt in with a blank import of
// github.com/xbcio/xbc/extensions/authentication/apikey/autoload; applications
// with a private catalog can use Definition directly.
package apikey
