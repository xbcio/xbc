package saml

import "errors"

var (
	// ErrInvalidConfig reports a static configuration that New refuses to
	// freeze into a provider. It never carries a key, a certificate body, an
	// assertion, or an attribute value.
	ErrInvalidConfig = errors.New("saml: invalid configuration")
	// ErrNilContext reports a nil context passed to Verify or LoadMetadata.
	ErrNilContext = errors.New("saml: nil context")
	// ErrNilProvider reports a nil or uninitialized provider.
	ErrNilProvider = errors.New("saml: nil provider")
	// ErrInvalidResponse reports a response that failed parsing, structural
	// validation, signature verification, or condition checks. Every such
	// failure wraps this sentinel, so errors.Is alone cannot tell the stages
	// apart; the wrapped reason exists for diagnostics, and a caller must not
	// expose it to end users. It never carries assertion text, a NameID, an
	// attribute value, a certificate, or a key.
	ErrInvalidResponse = errors.New("saml: invalid response")
	// ErrReplay reports an assertion ID that the replay cache has already
	// seen within its validity window.
	ErrReplay = errors.New("saml: assertion replay detected")
	// ErrUnavailable reports an operational failure: a metadata fetch, the
	// replay cache, or another dependency outside the response itself. It
	// always fails closed.
	ErrUnavailable = errors.New("saml: dependency unavailable")
)
