package authentication

import "fmt"

// Scheme is the stable, configuration-facing identity of an authentication
// mechanism. A scheme is compared exactly; it is never inferred from a Go type
// or normalized at runtime.
//
// Scheme values are intended to be declared as package constants:
//
//	const SchemeJWT authentication.Scheme = "jwt"
type Scheme string

// String returns the scheme's stable textual form.
func (s Scheme) String() string { return string(s) }

// Validate reports whether s is a legal stable scheme identifier. Legal
// identifiers contain only lowercase ASCII letters, digits, underscores, and
// hyphens.
func (s Scheme) Validate() error {
	if s == "" {
		return fmt.Errorf("%w: value cannot be empty", ErrInvalidScheme)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return fmt.Errorf("%w %q: invalid character %q", ErrInvalidScheme, s, r)
		}
	}
	return nil
}
