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
//
// ListBySubject, RevokeBySubject, and RevokeByDigest work on a subject's
// sessions without holding their IDs: the activity list behind a "my devices"
// page, signing a user out everywhere, and ending one listed session. They need
// a Store that implements IndexedStore, as the built-in memory and Redis stores
// do, and return ErrNotSupported otherwise. SessionInfo.Digest, like IDDigest,
// is a handle that names a session and cannot authenticate as it. Changing a
// subject's credentials and then calling RevokeBySubject in that order is what
// ends their sessions; a session created while the revocation runs may survive
// it. Sessions created before a deployment adopted the index are not indexed
// and expire on their own schedule. Each method returns ErrStopped once the
// plugin stopped and a parameter error for an empty subject.
type Manager interface {
	Create(ctx context.Context, subject string, attributes map[string]any) (Session, error)
	Rotate(ctx context.Context, id string) (Session, error)
	Revoke(ctx context.Context, id string) error
	// ListBySubject returns the live sessions of subject, in no particular order.
	ListBySubject(ctx context.Context, subject string) ([]SessionInfo, error)
	// RevokeBySubject deletes every live session of subject and returns how many
	// it deleted; it is idempotent.
	RevokeBySubject(ctx context.Context, subject string) (int, error)
	// RevokeByDigest deletes the session named by digest only if it belongs to
	// subject, and reports whether it deleted one. A malformed digest is an
	// error, distinct from an unknown or foreign one, which report false.
	RevokeByDigest(ctx context.Context, subject, digest string) (bool, error)
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

// indexed returns the store's subject-index capability, after the checks every
// index method shares: a stopped manager, a nil context, and an empty subject
// are refused before the store is asked anything.
func (m *manager) indexed(ctx context.Context, subject string) (IndexedStore, string, error) {
	if m.closed.Load() {
		return nil, "", ErrStopped
	}
	if err := contextError(ctx); err != nil {
		return nil, "", err
	}
	subject, err := normalizeSubject(subject)
	if err != nil {
		return nil, "", err
	}
	store, ok := m.store.(IndexedStore)
	if !ok {
		return nil, "", ErrNotSupported
	}
	return store, subject, nil
}

func (m *manager) ListBySubject(ctx context.Context, subject string) ([]SessionInfo, error) {
	store, subject, err := m.indexed(ctx, subject)
	if err != nil {
		return nil, err
	}
	return store.ListBySubject(ctx, subject)
}

func (m *manager) RevokeBySubject(ctx context.Context, subject string) (int, error) {
	store, subject, err := m.indexed(ctx, subject)
	if err != nil {
		return 0, err
	}
	return store.RevokeBySubject(ctx, subject)
}

func (m *manager) RevokeByDigest(ctx context.Context, subject, digest string) (bool, error) {
	store, subject, err := m.indexed(ctx, subject)
	if err != nil {
		return false, err
	}
	return store.RevokeByDigest(ctx, subject, digest)
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
