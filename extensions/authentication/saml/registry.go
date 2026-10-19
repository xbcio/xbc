package saml

import (
	"fmt"

	"github.com/xbcio/xbc/extensions/authentication/federation"
)

// ValidateProvider is the static validator for the SAML provider registry. It
// rejects a provider that was not constructed by New and a provider registered
// under a name other than its own: the name is recorded in every
// ExternalIdentity, so a mismatch would attribute logins to the wrong IdP.
//
// Validation is static. Loading IdP metadata is an explicit LoadMetadata call
// made by the application, so a rejected configuration leaves the previous one
// in effect and an unreachable IdP never blocks a registry write.
func ValidateProvider(name string, provider *Provider) error {
	if provider == nil {
		return fmt.Errorf("%w: provider is nil", ErrInvalidConfig)
	}
	if err := provider.usable(); err != nil {
		return err
	}
	if provider.name != name {
		return fmt.Errorf("%w: provider is named %q but was registered as %q", ErrInvalidConfig, provider.name, name)
	}
	return nil
}

// NewRegistry returns a SAML provider registry keyed by provider name. The
// registry validates every Put through ValidateProvider and replaces a
// provider atomically; a rejected Put leaves the previous provider in effect.
// Applications use federation.Registry directly when they need to compose
// validation with their own rules.
func NewRegistry() *federation.Registry[*Provider] {
	return federation.NewRegistry[*Provider](ValidateProvider)
}
