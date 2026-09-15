package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xbcio/xbc/transport/web/enginetest"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

func rawID(seed byte, size int) []byte {
	raw := make([]byte, size)
	for i := range raw {
		raw[i] = seed + byte(i)
	}
	return raw
}

func testManager(t *testing.T, store Store, now time.Time) *manager {
	t.Helper()
	cfg, err := normalizeConfig(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return &manager{store: store, cfg: cfg, now: func() time.Time { return now }}
}

func TestManagerCreateUsesHighEntropyOpaqueIDsAndRetriesCollisions(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	store := newControlledMemoryStore(t, &now)
	collision := testID(1, 32)
	if err := store.Create(context.Background(), sessionValue(collision, "existing", now, time.Hour), time.Minute); err != nil {
		t.Fatal(err)
	}

	m := testManager(t, store, now)
	m.random = bytes.NewReader(append(rawID(1, 32), rawID(90, 32)...))
	input := map[string]any{"role": "admin", "nested": map[string]any{"team": "core"}}
	created, err := m.Create(context.Background(), " alice ", input)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(created.ID)
	if err != nil || len(decoded) != 32 || created.ID == collision {
		t.Fatalf("created ID = %q, decoded bytes = %d, err = %v", created.ID, len(decoded), err)
	}
	if created.Subject != "alice" || !created.IssuedAt.Equal(now) || !created.ExpiresAt.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("created session = %#v", created)
	}
	input["role"] = "mutated"
	input["nested"].(map[string]any)["team"] = "other"
	stored, found, err := store.Get(context.Background(), created.ID)
	if err != nil || !found || stored.Attributes["role"] != "admin" || stored.Attributes["nested"].(map[string]any)["team"] != "core" {
		t.Fatalf("stored session = %#v, found=%v, err=%v", stored, found, err)
	}
}

func TestManagerCreateReportsRandomnessAndCollisionFailures(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	store := &recordingStore{}
	m := testManager(t, store, now)
	m.random = failingReader{}
	if _, err := m.Create(context.Background(), "alice", nil); err == nil {
		t.Fatal("Create with failed random source succeeded")
	}

	var calls int
	store.create = func(context.Context, Session, time.Duration) error {
		calls++
		return ErrAlreadyExists
	}
	m.random = bytes.NewReader(bytes.Repeat(rawID(2, 32), 4))
	if _, err := m.Create(context.Background(), "alice", nil); err == nil || calls != 4 {
		t.Fatalf("Create collision exhaustion calls=%d err=%v", calls, err)
	}
}

func TestManagerRotateAtomicallyRevokesOldIDAndPreservesLifetime(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	store := newControlledMemoryStore(t, &now)
	oldID := testID(3, 32)
	original := sessionValue(oldID, "alice", now.Add(-time.Hour), 12*time.Hour)
	if err := store.Create(context.Background(), original, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	m := testManager(t, store, now)
	m.random = bytes.NewReader(rawID(40, 32))
	replacement, err := m.Rotate(context.Background(), oldID)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID == oldID || !replacement.IssuedAt.Equal(original.IssuedAt) || !replacement.ExpiresAt.Equal(original.ExpiresAt) {
		t.Fatalf("replacement = %#v", replacement)
	}
	if _, found, _ := store.Get(context.Background(), oldID); found {
		t.Fatal("old fixation-prone ID remains usable")
	}
	if got, found, err := store.Get(context.Background(), replacement.ID); err != nil || !found || got.Subject != "alice" {
		t.Fatalf("replacement lookup = %#v, found=%v, err=%v", got, found, err)
	}
	if err := m.Revoke(context.Background(), replacement.ID); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := store.Get(context.Background(), replacement.ID); found {
		t.Fatal("revoked ID remains usable")
	}
}

func TestManagerCookieAttributesAndClearCookie(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	cfg := DefaultConfig()
	cfg.Name = "__Host-session"
	cfg.SameSite = "strict"
	cfg.TTL = 2 * time.Hour
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := &manager{store: &recordingStore{}, cfg: normalized, now: func() time.Time { return now }}
	value := sessionValue(testID(4, 32), "sensitive-subject", now, 90*time.Second)

	recorder := httptest.NewRecorder()
	ctx := enginetest.NewCtx(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if err := m.SetCookie(ctx, value); err != nil {
		t.Fatal(err)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %#v", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != "__Host-session" || cookie.Value != value.ID || cookie.Path != "/" || cookie.Domain != "" || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge != 90 {
		t.Fatalf("cookie = %#v", cookie)
	}
	if strings.Contains(recorder.Header().Get("Set-Cookie"), value.Subject) {
		t.Fatal("cookie leaked cleartext identity")
	}

	cleared := httptest.NewRecorder()
	clearContext := enginetest.NewCtx(cleared, httptest.NewRequest(http.MethodGet, "/", nil))
	m.ClearCookie(clearContext)
	clearCookies := cleared.Result().Cookies()
	if len(clearCookies) != 1 || clearCookies[0].Value != "" || clearCookies[0].MaxAge != -1 || !clearCookies[0].HttpOnly || !clearCookies[0].Secure {
		t.Fatalf("clear cookie = %#v", clearCookies)
	}
}

func TestManagerRejectsInvalidInputsAndUseAfterStop(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	m := testManager(t, &recordingStore{}, now)
	if _, err := m.Create(context.Background(), " ", nil); err == nil {
		t.Fatal("empty subject accepted")
	}
	if _, err := m.Create(context.Background(), "alice", map[string]any{"bad": make(chan int)}); err == nil {
		t.Fatal("non-JSON attribute accepted")
	}
	if _, err := m.Rotate(context.Background(), "attacker-selected"); !errors.Is(err, ErrInvalidCookie) {
		t.Fatalf("invalid Rotate error = %v", err)
	}
	if err := m.Revoke(context.Background(), "attacker-selected"); !errors.Is(err, ErrInvalidCookie) {
		t.Fatalf("invalid Revoke error = %v", err)
	}
	m.stop()
	if _, err := m.Create(context.Background(), "alice", nil); !errors.Is(err, ErrStopped) {
		t.Fatalf("Create after stop = %v", err)
	}
	if err := m.SetCookie(nil, sessionValue(testID(1, 32), "alice", now, time.Hour)); !errors.Is(err, ErrStopped) {
		t.Fatalf("SetCookie after stop = %v", err)
	}
}
