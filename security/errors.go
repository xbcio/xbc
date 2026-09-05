package security

import (
	"errors"
	"fmt"
)

var (
	// ErrInvalidScheme identifies an empty or syntactically invalid scheme.
	ErrInvalidScheme = errors.New("security: invalid scheme")
	// ErrDuplicateScheme identifies repeated authenticator, default, or selected
	// schemes.
	ErrDuplicateScheme = errors.New("security: duplicate scheme")
	// ErrUnknownScheme identifies a default or selected scheme with no registered
	// authenticator.
	ErrUnknownScheme = errors.New("security: unknown scheme")
	// ErrNoAuthenticators identifies a manager with no authenticators.
	ErrNoAuthenticators = errors.New("security: no authenticators")
	// ErrNoDefaultSchemes identifies a manager without a restrictive default.
	ErrNoDefaultSchemes = errors.New("security: no default schemes")
	// ErrEmptySelection identifies an explicit route selection with no schemes.
	ErrEmptySelection = errors.New("security: empty scheme selection")
	// ErrNilAuthenticator identifies a nil or typed-nil authenticator.
	ErrNilAuthenticator = errors.New("security: nil authenticator")
	// ErrNilCredentialSource identifies a nil or typed-nil credential source.
	ErrNilCredentialSource = errors.New("security: nil credential source")
	// ErrNilContext identifies a nil authentication context.
	ErrNilContext = errors.New("security: nil context")
	// ErrInvalidCredentialResult identifies a zero or internally inconsistent
	// extraction result.
	ErrInvalidCredentialResult = errors.New("security: invalid credential result")
	// ErrInvalidAuthenticatorResult identifies a zero or internally inconsistent
	// authenticator result.
	ErrInvalidAuthenticatorResult = errors.New("security: invalid authenticator result")
)

// Operation identifies the stage of an operational authentication failure.
type Operation uint8

const (
	// OperationCredentialCollection means a credential source failed.
	OperationCredentialCollection Operation = iota + 1
	// OperationAuthentication means an authenticator failed.
	OperationAuthentication
)

// String returns a stable, non-sensitive name for the operation.
func (o Operation) String() string {
	switch o {
	case OperationCredentialCollection:
		return "credential collection"
	case OperationAuthentication:
		return "authentication"
	default:
		return "authentication operation"
	}
}

// OperationalError wraps an extraction or verification failure without
// rendering the underlying error text. Error therefore remains safe for normal
// request diagnostics even if the cause accidentally contains credential data.
// The cause remains available through errors.Is/errors.As and Unwrap for trusted
// internal handling.
type OperationalError struct {
	operation Operation
	scheme    Scheme
	cause     error
}

func newOperationalError(operation Operation, scheme Scheme, cause error) *OperationalError {
	return &OperationalError{operation: operation, scheme: scheme, cause: cause}
}

// Error returns a safe diagnostic containing only the operation and stable
// scheme. It intentionally omits the wrapped cause text.
func (e *OperationalError) Error() string {
	if e == nil {
		return "security: operational authentication failure"
	}
	return fmt.Sprintf("security: %s failed for scheme %q", e.operation, e.scheme)
}

// Unwrap returns the underlying operational cause.
func (e *OperationalError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Operation returns the failed operation.
func (e *OperationalError) Operation() Operation {
	if e == nil {
		return 0
	}
	return e.operation
}

// Scheme returns the scheme being processed when the failure occurred.
func (e *OperationalError) Scheme() Scheme {
	if e == nil {
		return ""
	}
	return e.scheme
}
