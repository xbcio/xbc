package ldap

import "errors"

var (
	// ErrInvalidConfig reports a configuration that violates a static rule.
	// New and ValidateProvider reject it before any network call, and a
	// rejected registry Put leaves the previous configuration in effect.
	ErrInvalidConfig = errors.New("ldap: invalid configuration")
	// ErrNilContext reports a nil context passed to Verify.
	ErrNilContext = errors.New("ldap: nil context")
	// ErrNilProvider reports a method call on a nil *Provider.
	ErrNilProvider = errors.New("ldap: nil provider")
	// ErrInvalidCredentials reports a failed login. It is the single outward
	// result of every credential outcome: an unknown user, a wrong password,
	// a directory entry that matches more than one user, and an empty user
	// name or password are indistinguishable to the caller. The caller must
	// map it onto its own generic login failure.
	ErrInvalidCredentials = errors.New("ldap: invalid credentials")
	// ErrUnavailable reports an operational failure: DNS, connection, TLS,
	// timeout, service-account bind, or an unexpected directory result. It
	// never carries directory text or credential material.
	ErrUnavailable = errors.New("ldap: directory unavailable")
	// ErrAddressRejected reports a directory host or a resolved address
	// outside the egress address policy, including a mixed public/private
	// answer. It is a policy refusal, not a credential outcome.
	ErrAddressRejected = errors.New("ldap: address rejected by policy")
	// ErrInvalidDirectoryEntry reports a search result that cannot yield a
	// verified identity: a missing, empty, or ambiguous subject attribute, or
	// a configured claim attribute that is absent or not valid UTF-8.
	ErrInvalidDirectoryEntry = errors.New("ldap: invalid directory entry")
)
