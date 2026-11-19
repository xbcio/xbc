package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrNotFound reports that a session is absent or no longer live.
	ErrNotFound = errors.New("session: session not found")
	// ErrAlreadyExists reports a generated identifier collision.
	ErrAlreadyExists = errors.New("session: session already exists")
	// ErrInvalidCookie reports an ambiguous or malformed session cookie.
	ErrInvalidCookie = errors.New("session: invalid session cookie")
	// ErrStopped reports use after the owning plugin or store was stopped.
	ErrStopped = errors.New("session: stopped")
	// ErrNotSupported reports an operation the configured Store does not
	// implement: the subject-index methods of Manager need an IndexedStore,
	// UpdateAttributes and PatchAttributes of AttributeManager need an
	// AttributeStore and a PatchStore respectively, and a configured
	// max_sessions needs an IndexedStore.
	ErrNotSupported = errors.New("session: operation not supported by the store")
	// ErrConflict reports that an attribute patch could not be applied within
	// the store's bounded retry budget because concurrent writers kept changing
	// the session. The session is unchanged by that attempt, and the caller may
	// retry the whole patch.
	ErrConflict = errors.New("session: concurrent update conflict")
	// ErrTooManySessions reports that the configured max_sessions limit refused
	// a new session under the reject_new policy.
	ErrTooManySessions = errors.New("session: too many concurrent sessions")
)

// Verdict is AttributeManager.Verify's internal classification of a presented
// session ID. It exists for host-side auditing, metrics, and diagnostics and
// must never be returned to a client: telling a caller whether an ID was
// malformed, unknown, or lost to a store failure turns the session check into
// an oracle for which IDs exist. The authentication adapter collapses every
// non-valid verdict into one rejection reason.
//
// The zero value is Malformed so a Verdict that was never assigned never
// reads as Valid.
type Verdict int

const (
	// VerdictMalformed reports an ID that fails the shape check (validID)
	// shared by Create, Rotate, and Revoke; no store lookup is performed.
	VerdictMalformed Verdict = iota
	// VerdictValid reports a live session. Verify has already renewed its
	// idle lease, subject to touch_interval throttling.
	VerdictValid
	// VerdictNotFound reports a well-formed ID the store did not resolve. An
	// expired session, an evicted one, a revoked one, and an ID that never
	// existed are all indistinguishable here and must stay so toward clients.
	VerdictNotFound
	// VerdictStoreError reports that the store could not answer. Verify
	// returns the underlying error alongside this verdict.
	VerdictStoreError
)

func (v Verdict) String() string {
	switch v {
	case VerdictMalformed:
		return "malformed"
	case VerdictValid:
		return "valid"
	case VerdictNotFound:
		return "not_found"
	case VerdictStoreError:
		return "store_error"
	default:
		return fmt.Sprintf("Verdict(%d)", int(v))
	}
}

// Session is the authenticated server-side session value. ID is the opaque
// identifier also carried by the cookie; Attributes must contain JSON-safe
// values. Every API returning a Session returns a defensive Attributes copy.
type Session struct {
	ID         string
	Subject    string
	Attributes map[string]any
	IssuedAt   time.Time
	ExpiresAt  time.Time
}

func normalizeSession(value Session) (Session, error) {
	value.ID = strings.TrimSpace(value.ID)
	value.Subject = strings.TrimSpace(value.Subject)
	if value.ID == "" {
		return Session{}, errors.New("session: ID cannot be empty")
	}
	if value.Subject == "" {
		return Session{}, errors.New("session: subject cannot be empty")
	}
	if value.IssuedAt.IsZero() || value.ExpiresAt.IsZero() || !value.ExpiresAt.After(value.IssuedAt) {
		return Session{}, errors.New("session: expiration must be after issue time")
	}
	attributes, err := normalizeAttributes(value.Attributes)
	if err != nil {
		return Session{}, err
	}
	value.Attributes = attributes
	value.IssuedAt = value.IssuedAt.UTC().Truncate(time.Millisecond)
	value.ExpiresAt = value.ExpiresAt.UTC().Truncate(time.Millisecond)
	return value, nil
}

func normalizeAttributes(attributes map[string]any) (map[string]any, error) {
	if attributes == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(attributes)
	if err != nil {
		return nil, fmt.Errorf("session: attributes must be JSON-safe: %w", err)
	}
	var normalized map[string]any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return nil, fmt.Errorf("session: decode normalized attributes: %w", err)
	}
	return normalized, nil
}

func cloneSession(value Session) Session {
	value.Attributes = cloneMap(value.Attributes)
	return value
}

func cloneMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	copy := make(map[string]any, len(source))
	for key, value := range source {
		copy[key] = cloneValue(value)
	}
	return copy
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
	case []any:
		copy := make([]any, len(typed))
		for i := range typed {
			copy[i] = cloneValue(typed[i])
		}
		return copy
	case []string:
		return append([]string(nil), typed...)
	case []byte:
		return append([]byte(nil), typed...)
	default:
		return value
	}
}

// validatePatchArguments rejects a patch that names one attribute as both set
// and unset. The order between the two halves would otherwise be the only
// thing deciding the outcome, which no caller can rely on by accident.
func validatePatchArguments(set map[string]any, unset []string) error {
	for _, key := range unset {
		if _, both := set[key]; both {
			return fmt.Errorf("session: attribute %q cannot be both set and unset", key)
		}
	}
	return nil
}

// patchedAttributes returns a new attributes map with every unset key removed
// and every set key assigned. A nil current map with no set keys stays nil, so
// a patch that only removes absent keys never turns "no attributes" into a
// stored empty object.
func patchedAttributes(current, set map[string]any, unset []string) map[string]any {
	if current == nil && len(set) == 0 {
		return nil
	}
	merged := cloneMap(current)
	if merged == nil {
		merged = make(map[string]any, len(set))
	}
	for _, key := range unset {
		delete(merged, key)
	}
	for key, value := range set {
		merged[key] = cloneValue(value)
	}
	return merged
}

// patchKeyCount is the number of distinct attribute keys a patch names. It is
// the Count an attributes-patched event carries: a size, never a key name or a
// value.
func patchKeyCount(set map[string]any, unset []string) int {
	keys := make(map[string]struct{}, len(set)+len(unset))
	for key := range set {
		keys[key] = struct{}{}
	}
	for _, key := range unset {
		keys[key] = struct{}{}
	}
	return len(keys)
}
