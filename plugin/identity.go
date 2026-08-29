package plugin

import "fmt"

// Key is the stable, configuration-facing identity of a plugin. It is the
// only value used to name Definitions, dependency references, Contexts,
// configuration sections, and diagnostics. A Key is never derived from a Go
// package path or concrete type.
type Key string

// String returns the key's textual form.
func (k Key) String() string { return string(k) }

// Validate reports whether k is a legal plugin key.
func (k Key) Validate() error { return validateIdentifier("plugin key", string(k)) }

// Ref returns a hard dependency reference to k.
func (k Key) Ref() Ref { return Ref{key: k} }

// DefaultInstance is the canonical instance name used when no named instance
// was requested.
const DefaultInstance = "default"

// NormalizeInstance folds the empty spelling onto DefaultInstance.
func NormalizeInstance(s string) string {
	if s == "" {
		return DefaultInstance
	}
	return s
}

// Identity names one live plugin instance.
type Identity struct {
	Plugin   Key
	Instance string
}

// String renders "gorm" for the default instance and
// "gorm[readonly]" for a named instance.
func (i Identity) String() string {
	if i.Instance == "" || i.Instance == DefaultInstance {
		return i.Plugin.String()
	}
	return i.Plugin.String() + "[" + i.Instance + "]"
}

func validateIdentifier(kind, s string) error {
	if s == "" {
		return fmt.Errorf("xbc: %s cannot be empty", kind)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return fmt.Errorf("xbc: %s %q contains invalid character %q, only lowercase letters, digits, underscores, and hyphens are allowed", kind, s, r)
		}
	}
	return nil
}

// ValidateName is retained as a convenience for callers validating textual
// input before converting it to Key.
func ValidateName(s string) error { return Key(s).Validate() }

// ValidateInstanceName validates a configured instance name.
func ValidateInstanceName(s string) error { return validateIdentifier("instance name", s) }
