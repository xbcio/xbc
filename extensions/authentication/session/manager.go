package session

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"time"
)

// Manager is the protocol-neutral session API. Create and Rotate return the
// value whose opaque ID a transport adapter hands to the client (the Web
// adapter installs it as a cookie). Rotate preserves the original absolute
// lifetime and revokes the previous ID; RedisStore.Rotate documents the window
// in which both IDs remain valid while a rotation is in flight.
type Manager interface {
	Create(ctx context.Context, subject string, attributes map[string]any) (Session, error)
	Rotate(ctx context.Context, id string) (Session, error)
	Revoke(ctx context.Context, id string) error
}

type manager struct {
	store  Store
	cfg    normalizedConfig
	now    func() time.Time
	random io.Reader
	closed atomic.Bool
}

func (m *manager) clock() time.Time {
	if m.now == nil {
		return time.Now().UTC()
	}
	return m.now().UTC()
}

func (m *manager) Create(ctx context.Context, subject string, attributes map[string]any) (Session, error) {
	if m.closed.Load() {
		return Session{}, ErrStopped
	}
	if err := contextError(ctx); err != nil {
		return Session{}, err
	}
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return Session{}, errors.New("session: subject cannot be empty")
	}
	normalizedAttributes, err := normalizeAttributes(attributes)
	if err != nil {
		return Session{}, err
	}
	now := m.clock().Truncate(time.Millisecond)
	for range 4 {
		id, err := m.newID()
		if err != nil {
			return Session{}, err
		}
		value := Session{ID: id, Subject: subject, Attributes: normalizedAttributes, IssuedAt: now, ExpiresAt: now.Add(m.cfg.ttl)}
		err = m.store.Create(ctx, value, m.cfg.idleTTL)
		if errors.Is(err, ErrAlreadyExists) {
			continue
		}
		if err != nil {
			return Session{}, err
		}
		return cloneSession(value), nil
	}
	return Session{}, errors.New("session: could not allocate a unique session ID")
}

func (m *manager) Rotate(ctx context.Context, id string) (Session, error) {
	if m.closed.Load() {
		return Session{}, ErrStopped
	}
	if !validID(id, m.cfg.idBytes) {
		return Session{}, ErrInvalidCookie
	}
	current, found, err := m.store.Get(ctx, id)
	if err != nil {
		return Session{}, err
	}
	if !found {
		return Session{}, ErrNotFound
	}
	for range 4 {
		newID, err := m.newID()
		if err != nil {
			return Session{}, err
		}
		replacement := cloneSession(current)
		replacement.ID = newID
		err = m.store.Rotate(ctx, id, replacement, m.cfg.idleTTL)
		if errors.Is(err, ErrAlreadyExists) {
			continue
		}
		if err != nil {
			return Session{}, err
		}
		return replacement, nil
	}
	return Session{}, errors.New("session: could not allocate a unique session ID")
}

func (m *manager) Revoke(ctx context.Context, id string) error {
	if m.closed.Load() {
		return ErrStopped
	}
	if !validID(id, m.cfg.idBytes) {
		return ErrInvalidCookie
	}
	return m.store.Delete(ctx, id)
}

// lifetime reports how long value's ID may still be handed to a client: a
// stopped manager, a malformed ID, and an already expired session are all
// refused.
func (m *manager) lifetime(value Session) (time.Duration, error) {
	if m.closed.Load() {
		return 0, ErrStopped
	}
	if !validID(value.ID, m.cfg.idBytes) || !value.ExpiresAt.After(m.clock()) {
		return 0, ErrInvalidCookie
	}
	remaining := time.Until(value.ExpiresAt)
	if m.now != nil {
		remaining = value.ExpiresAt.Sub(m.clock())
	}
	if remaining <= 0 {
		return 0, ErrInvalidCookie
	}
	return remaining, nil
}

func (m *manager) newID() (string, error) {
	raw := make([]byte, m.cfg.idBytes)
	reader := m.random
	if reader == nil {
		reader = rand.Reader
	}
	if _, err := io.ReadFull(reader, raw); err != nil {
		return "", errors.New("session: generate random session ID")
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (m *manager) stop() { m.closed.Store(true) }

func validID(id string, bytes int) bool {
	if id == "" || id != strings.TrimSpace(id) || strings.Contains(id, "=") {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(id)
	return err == nil && len(raw) == bytes
}
