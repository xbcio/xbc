package federation

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	// AttributeProtocol is the reserved session attribute holding the protocol
	// name ("oidc", "oauth2", "saml", or "ldap"). The protocol name is
	// deliberately not stored as an authentication method: Principal.AuthMethod
	// remains "session" for every session.
	AttributeProtocol = "idp_protocol"
	// AttributeProvider is the reserved session attribute holding the provider
	// identifier chosen by the application.
	AttributeProvider = "idp"
	// AttributeIssuer is the reserved session attribute holding the issuer or
	// directory identifier reported by the protocol module.
	AttributeIssuer = "idp_issuer"
	// AttributeAuthTime is the reserved session attribute holding the instant
	// the external authentication happened. It is always a UTC time.Time.
	AttributeAuthTime = "auth_time"
)

// MaxSessionAttributesBytes is the maximum encoded size of the session
// attributes produced by SessionAttributes. The limit bounds what a single
// login can push into the session store.
const MaxSessionAttributesBytes = 16 << 10

var (
	// ErrReservedAttribute reports an application attribute that uses one of
	// the reserved keys. Reserved keys are owned by xbc and must not be
	// overridden.
	ErrReservedAttribute = errors.New("federation: application attribute uses a reserved key")
	// ErrInvalidAttribute reports attributes that cannot be encoded as JSON,
	// and therefore cannot be stored in a session.
	ErrInvalidAttribute = errors.New("federation: attributes must be JSON-safe")
	// ErrAttributeSize reports merged session attributes above
	// MaxSessionAttributesBytes.
	ErrAttributeSize = errors.New("federation: session attributes exceed the size limit")
)

// IsReservedAttribute reports whether key is a session attribute appended by
// xbc rather than by the application.
func IsReservedAttribute(key string) bool {
	switch key {
	case AttributeProtocol, AttributeProvider, AttributeIssuer, AttributeAuthTime:
		return true
	default:
		return false
	}
}

// SessionAttributes merges the application's session attributes with the
// reserved attributes derived from identity.
//
// attributes must not contain a reserved key; a collision is an operational
// failure rather than an override. The result is a defensive copy, is
// validated as JSON-safe, and must encode to at most
// MaxSessionAttributesBytes. auth_time is normalized to UTC.
func SessionAttributes(identity ExternalIdentity, attributes map[string]any) (map[string]any, error) {
	for key := range attributes {
		if IsReservedAttribute(key) {
			return nil, fmt.Errorf("%w: %q", ErrReservedAttribute, key)
		}
	}

	merged := cloneAttributes(attributes)
	if merged == nil {
		merged = make(map[string]any, 4)
	}
	merged[AttributeProtocol] = identity.Protocol
	merged[AttributeProvider] = identity.Provider
	merged[AttributeIssuer] = identity.Issuer
	merged[AttributeAuthTime] = identity.AuthTime.UTC()

	encoded, err := marshalAttributes(merged)
	if err != nil {
		return nil, err
	}
	if len(encoded) > MaxSessionAttributesBytes {
		return nil, fmt.Errorf("%w: encoded %d bytes, limit %d", ErrAttributeSize, len(encoded), MaxSessionAttributesBytes)
	}
	return merged, nil
}

// marshalAttributes encodes attributes as JSON, mapping every encoding failure
// to ErrInvalidAttribute. The wrapped cause describes the unsupported type or
// value shape, never a surrounding secret.
func marshalAttributes(attributes map[string]any) ([]byte, error) {
	encoded, err := json.Marshal(attributes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidAttribute, err)
	}
	return encoded, nil
}

// cloneAttributes returns a defensive copy of a JSON-safe attribute map. It is
// nil-preserving so callers can tell "no attributes" from an empty map.
func cloneAttributes(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	copied := make(map[string]any, len(source))
	for key, value := range source {
		copied[key] = cloneAttributeValue(value)
	}
	return copied
}

// cloneAttributeValue copies the container shapes a session commonly stores,
// recursively:
//
//   - map[string]any
//   - map[string]string
//   - map[string][]string (each slice is copied as well)
//   - []any
//   - []string
//   - []byte
//
// Every other value is treated as immutable; an application must not hand over
// a mutable custom type and then mutate it. The list is deliberate rather than
// reflective: reflection cannot copy unexported fields safely, and a JSON
// round-trip would rewrite the value types the application chose.
func cloneAttributeValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneAttributes(typed)
	case map[string]string:
		copied := make(map[string]string, len(typed))
		for key, item := range typed {
			copied[key] = item
		}
		return copied
	case map[string][]string:
		copied := make(map[string][]string, len(typed))
		for key, item := range typed {
			copied[key] = append([]string(nil), item...)
		}
		return copied
	case []any:
		copied := make([]any, len(typed))
		for index := range typed {
			copied[index] = cloneAttributeValue(typed[index])
		}
		return copied
	case []string:
		return append([]string(nil), typed...)
	case []byte:
		return append([]byte(nil), typed...)
	default:
		return value
	}
}
