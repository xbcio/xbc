// Package apikey extracts API keys from gRPC call metadata for XBC's gRPC
// transport. Verification, configuration, and the digest repository belong to
// the neutral authenticator at
// github.com/xbcio/xbc/extensions/authentication/apikey; this package only
// reads the configured key, app-ID, and authorization metadata entries --
// the authenticator's header names, lower-cased as gRPC delivers them -- and
// hands a PresentedKey to that authenticator.
//
// # Usage
//
// Configure plugins.apikey and compose Bundle explicitly; it includes the
// neutral authenticator. The same plugins.apikey section serves the Web
// extractor too, so a process exposing both transports verifies keys against
// one repository. Bundle and ordinary package imports are side-effect free;
// executables that use XBC's optional process-wide composition can import the
// leaf apikey/autoload package instead.
//
// Whether a method is public or protected is decided by grpc.security and the
// registering plugin's Methods declaration, not by this plugin. A handler of
// an authenticated call reads the shared principal:
//
//	principal, ok := grpc.PrincipalFromContext(ctx)
//	if !ok || principal.AuthMethod != "apikey" {
//		return nil, status.Error(codes.Unauthenticated, "unauthenticated")
//	}
package apikey
