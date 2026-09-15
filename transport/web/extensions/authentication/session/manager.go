package session

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/xbcio/xbc/transport/web"
)

// Manager is the handler-facing session API. Create and Rotate return the
// value whose opaque ID should be installed with SetCookie. Rotate preserves
// the original absolute lifetime and atomically revokes the previous ID.
type Manager interface {
	Create(ctx context.Context, subject string, attributes map[string]any) (Session, error)
	Rotate(ctx context.Context, id string) (Session, error)
	Revoke(ctx context.Context, id string) error
	SetCookie(c *web.Ctx, value Session) error
	ClearCookie(c *web.Ctx)
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

func (m *manager) SetCookie(c *web.Ctx, value Session) error {
	if m.closed.Load() {
		return ErrStopped
	}
	if c == nil || c.Writer() == nil {
		return errors.New("session: cookie context cannot be nil")
	}
	if !validID(value.ID, m.cfg.idBytes) || !value.ExpiresAt.After(m.clock()) {
		return ErrInvalidCookie
	}
	remaining := time.Until(value.ExpiresAt)
	if m.now != nil {
		remaining = value.ExpiresAt.Sub(m.clock())
	}
	maxAge := int((remaining + time.Second - 1) / time.Second)
	if maxAge < 1 {
		return ErrInvalidCookie
	}
	http.SetCookie(c.Writer(), &http.Cookie{
		Name:     m.cfg.name,
		Value:    value.ID,
		Path:     m.cfg.path,
		Domain:   m.cfg.domain,
		Expires:  value.ExpiresAt,
		MaxAge:   maxAge,
		HttpOnly: m.cfg.httpOnly,
		Secure:   m.cfg.secure,
		SameSite: m.cfg.sameSite,
	})
	return nil
}

func (m *manager) ClearCookie(c *web.Ctx) {
	if c == nil || c.Writer() == nil {
		return
	}
	http.SetCookie(c.Writer(), &http.Cookie{
		Name:     m.cfg.name,
		Value:    "",
		Path:     m.cfg.path,
		Domain:   m.cfg.domain,
		Expires:  time.Unix(1, 0).UTC(),
		MaxAge:   -1,
		HttpOnly: m.cfg.httpOnly,
		Secure:   m.cfg.secure,
		SameSite: m.cfg.sameSite,
	})
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
