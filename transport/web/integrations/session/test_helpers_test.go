package session

import (
	"context"
	"testing"
	"time"

	"github.com/xbcio/xbc/config"
)

func sessionEnvironment(t *testing.T, otherPlugins map[string]any, sessionConfig map[string]any) *config.Environment {
	t.Helper()
	plugins := map[string]any{"session": sessionConfig}
	for key, value := range otherPlugins {
		plugins[key] = value
	}
	environment, err := config.NewEnvironment(map[string]any{"plugins": plugins}, "")
	if err != nil {
		t.Fatal(err)
	}
	return environment
}

func sessionValue(id, subject string, issued time.Time, ttl time.Duration) Session {
	return Session{
		ID:         id,
		Subject:    subject,
		Attributes: map[string]any{"role": "admin", "nested": map[string]any{"team": "core"}},
		IssuedAt:   issued,
		ExpiresAt:  issued.Add(ttl),
	}
}

type recordingStore struct {
	create func(context.Context, Session, time.Duration) error
	get    func(context.Context, string) (Session, bool, error)
	touch  func(context.Context, string, time.Duration, time.Duration) (Session, bool, error)
	rotate func(context.Context, string, Session, time.Duration) error
	delete func(context.Context, string) error
}

func (s *recordingStore) Create(ctx context.Context, value Session, ttl time.Duration) error {
	if s != nil && s.create != nil {
		return s.create(ctx, value, ttl)
	}
	return nil
}

func (s *recordingStore) Get(ctx context.Context, id string) (Session, bool, error) {
	if s != nil && s.get != nil {
		return s.get(ctx, id)
	}
	return Session{}, false, nil
}

func (s *recordingStore) Touch(ctx context.Context, id string, ttl, interval time.Duration) (Session, bool, error) {
	if s != nil && s.touch != nil {
		return s.touch(ctx, id, ttl, interval)
	}
	return Session{}, false, nil
}

func (s *recordingStore) Rotate(ctx context.Context, oldID string, replacement Session, ttl time.Duration) error {
	if s != nil && s.rotate != nil {
		return s.rotate(ctx, oldID, replacement, ttl)
	}
	return ErrNotFound
}

func (s *recordingStore) Delete(ctx context.Context, id string) error {
	if s != nil && s.delete != nil {
		return s.delete(ctx, id)
	}
	return nil
}
