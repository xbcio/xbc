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
)

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
