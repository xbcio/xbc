// Package apikey extracts API keys from HTTP requests for XBC's Web
// transport. Verification, configuration, and the digest repository belong to
// the neutral authenticator at
// github.com/xbcio/xbc/extensions/authentication/apikey; this package only
// reads the configured key, app-ID, and Authorization headers and hands a
// PresentedKey to that authenticator.
//
// # Usage
//
// The extractor plugs into the Web transport's built-in authentication
// middleware, which is the only place the live request is available. Configure
// plugins.apikey and compose Bundle explicitly; it includes the neutral
// authenticator, so a composition root never names both halves. Bundle and
// ordinary package imports are side-effect free. Executables that
// intentionally use XBC's optional process-wide composition can import the
// leaf apikey/autoload package instead.
//
// Whether a route is public or protected is decided by the web.security policy
// and the route's own .Auth() declaration, not by this plugin. On a
// successfully authenticated protected request, consume the shared Web
// principal rather than reading the credential header again:
//
//	principal, ok := web.CurrentPrincipal(c)
//	if !ok || principal.AuthMethod != "apikey" {
//		c.Status(http.StatusUnauthorized)
//		c.Abort()
//		return nil
//	}
//	c.JSON(http.StatusOK, map[string]any{"subject": principal.Subject})
//
// # Customized variants
//
// The extractor takes its scheme, headers, and app-ID handling from the core
// Plugin it pairs with, so a host can run a variant beside the default:
//
//	core, err := apikeycore.New(cfg, apikeycore.WithScheme("mcp-token"))
//	extractor, err := apikey.New(core)
//
// Define the pair as two host Definitions under the host's own keys (the
// core exporting authentication.Authenticator, the extractor exporting
// web.CredentialExtractor) and declare both schemes in web.security.schemes.
// The configuration keys (header, allow_bearer: false, ignore_app_id: true,
// min_key_bytes with a floor of 16, static sha256 and subject) are described
// in the core package documentation. With ignore_app_id the X-App-ID header
// is never read, so a repeated or blank value cannot affect the variant.
package apikey
