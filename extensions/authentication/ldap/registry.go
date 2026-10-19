package ldap

import (
	"fmt"

	"github.com/xbcio/xbc/extensions/authentication/federation"
)

// ValidateProvider is the static validator for the LDAP provider registry. It
// rejects a provider that was not constructed by New and a provider registered
// under a name other than its own: the name is recorded in every
// ExternalIdentity, so a mismatch would attribute logins to the wrong
// directory.
//
// Validation is static. No connection or search is made, so an unreachable
// directory never blocks a registry write and a rejected configuration leaves
// the previous one in effect.
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

// NewRegistry returns an LDAP provider registry keyed by provider name. The
// registry validates every Put through ValidateProvider and replaces a provider
// atomically; a rejected Put leaves the previous provider in effect. Remove
// affects only subsequent logins: sessions created earlier stay valid, and
// revoking them is the application's job.
func NewRegistry() *federation.Registry[*Provider] {
	return federation.NewRegistry[*Provider](ValidateProvider)
}
