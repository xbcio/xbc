package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication"
	sessioncore "github.com/xbcio/xbc/extensions/authentication/session"
	"github.com/xbcio/xbc/log"
)

const (
	sensitiveAttribute = "SENSITIVE-ATTRIBUTE-VALUE-MARKER"
	sensitiveSessionID = "SENSITIVE-SESSION-ID-MARKER"
)

type capturedLogEntry struct {
	level string
	body  string
}

type captureLogger struct {
	mu      sync.Mutex
	entries []capturedLogEntry
}

func (logger *captureLogger) record(level, message string, kv []any) {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	logger.entries = append(logger.entries, capturedLogEntry{
		level: level,
		body:  fmt.Sprintf("%s %v", message, kv),
	})
}

func (logger *captureLogger) Debug(message string, kv ...any) { logger.record("debug", message, kv) }
func (logger *captureLogger) Info(message string, kv ...any)  { logger.record("info", message, kv) }
func (logger *captureLogger) Warn(message string, kv ...any)  { logger.record("warn", message, kv) }
func (logger *captureLogger) Error(message string, kv ...any) { logger.record("error", message, kv) }
func (*captureLogger) Fatal(string, ...any)                   {}
func (logger *captureLogger) With(...any) log.Logger          { return logger }
func (*captureLogger) Enabled(log.Level) bool                 { return true }

func (logger *captureLogger) snapshot() []capturedLogEntry {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	return append([]capturedLogEntry(nil), logger.entries...)
}

type recordingObserver struct {
	mu     sync.Mutex
	events []sessioncore.Event
}

func (observer *recordingObserver) Observe(_ context.Context, event sessioncore.Event) {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	observer.events = append(observer.events, event)
}

func (observer *recordingObserver) snapshot() []sessioncore.Event {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return append([]sessioncore.Event(nil), observer.events...)
}

func captureLogs(t *testing.T) *captureLogger {
	t.Helper()
	previous := log.L()
	capture := &captureLogger{}
	log.SetLogger(capture)
	t.Cleanup(func() { log.SetLogger(previous) })
	return capture
}

func observedSessionPlugin(t *testing.T, observer sessioncore.Observer) (*Plugin, *sessioncore.Plugin) {
	t.Helper()
	config := sessioncore.DefaultConfig()
	config.Name = "sid"
	config.Secure = false
	core, err := sessioncore.New(config, sessioncore.WithObserver(observer))
	if err != nil {
		t.Fatalf("sessioncore.New() error = %v", err)
	}
	t.Cleanup(func() { _ = core.Stop(context.Background()) })
	adapter, err := New(core)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return adapter, core
}

func assertNoSensitiveText(t *testing.T, label, text string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(text, secret) {
			t.Fatalf("%s leaked sensitive value %q: %q", label, secret, text)
		}
	}
}

// TestSensitiveValuesDoNotReachLogsResponsesErrorsOrEvents drives every helper
// with sessions and attributes built from marker strings, then inspects each
// sink this package can write to. The Set-Cookie header is deliberately not in
// scope: carrying the opaque ID there is the cookie's job.
func TestSensitiveValuesDoNotReachLogsResponsesErrorsOrEvents(t *testing.T) {
	observer := &recordingObserver{}
	capture := captureLogs(t)
	adapter, core := observedSessionPlugin(t, observer)

	loginContext, loginRecorder := newTestContext(t, nil)
	value, err := adapter.Login(context.Background(), loginContext, "alice", map[string]any{
		"secret_attribute": sensitiveAttribute,
		"nested":           map[string]any{"note": sensitiveAttribute},
	})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	secrets := []string{value.ID, sensitiveAttribute, sensitiveSessionID}
	assertNoSensitiveText(t, "login response body", loginRecorder.Body.String(), secrets...)
	assertNoSensitiveText(t, "login error", fmt.Sprint(err), secrets...)

	extracted, err := adapter.ExtractCredential(newTestContextWithCookies(t, []*http.Cookie{
		{Name: "sid", Value: sensitiveSessionID},
	}))
	if err != nil {
		t.Fatalf("ExtractCredential() error = %v", err)
	}
	credential, ok := extracted.Credential()
	if !ok {
		t.Fatal("Credential() ok = false")
	}
	result, err := core.Authenticate(context.Background(), credential)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	reason, _ := result.Reason()
	assertNoSensitiveText(t, "rejection reason", string(reason), secrets...)
	assertNoSensitiveText(t, "rejection rendering", fmt.Sprintf("%+v %v", result, result), secrets...)

	refreshContext, refreshRecorder := newTestContext(t, []*http.Cookie{
		{Name: "sid", Value: value.ID},
	})
	_, refreshErr := adapter.Refresh(context.Background(), refreshContext, map[string]any{
		"secret_attribute": make(chan int),
	})
	if refreshErr == nil {
		t.Fatal("Refresh() accepted a non-JSON attribute value")
	}
	assertNoSensitiveText(t, "refresh error", refreshErr.Error(), secrets...)
	assertNoSensitiveText(t, "refresh response body", refreshRecorder.Body.String(), secrets...)

	failing := &fixedCarrier{
		result:   authentication.Absent(),
		writable: true,
		issueErr: errors.New("carrier write failed"),
	}
	failingAdapter, err := New(core, WithCarriers(failing))
	if err != nil {
		t.Fatalf("New(WithCarriers) error = %v", err)
	}
	failingContext, failingRecorder := newTestContext(t, nil)
	_, loginErr := failingAdapter.Login(context.Background(), failingContext, "alice", map[string]any{
		"secret_attribute": sensitiveAttribute,
	})
	if loginErr == nil {
		t.Fatal("Login() over a failing carrier = nil error")
	}
	if len(failing.issued) != 1 {
		t.Fatalf("carrier saw %d sessions, want 1", len(failing.issued))
	}
	assertNoSensitiveText(t, "failed login error", loginErr.Error(), failing.issued[0].ID, sensitiveAttribute)
	assertNoSensitiveText(t, "failed login response body", failingRecorder.Body.String(), failing.issued[0].ID, sensitiveAttribute)

	logoutContext, logoutRecorder := newTestContext(t, []*http.Cookie{{Name: "sid", Value: value.ID}})
	if err := adapter.Logout(context.Background(), logoutContext); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	assertNoSensitiveText(t, "logout response body", logoutRecorder.Body.String(), secrets...)

	for _, entry := range capture.snapshot() {
		assertNoSensitiveText(t, "log entry ("+entry.level+")", entry.body, secrets...)
	}
	events := observer.snapshot()
	if len(events) == 0 {
		t.Fatal("the observer saw no events, so the event redaction check proved nothing")
	}
	for _, event := range events {
		rendered := fmt.Sprintf("%+v %v", event, event)
		assertNoSensitiveText(t, "observer event", rendered, secrets...)
		if event.Digest == "" {
			continue
		}
		if len(event.Digest) != 64 {
			t.Fatalf("observer digest %q is not the fixed-width handle", event.Digest)
		}
		if event.Digest == value.ID || event.Digest == sensitiveSessionID {
			t.Fatalf("observer event carried a raw session ID: %+v", event)
		}
	}
}

// TestCurrentViewNeverRendersTheSessionID proves the view type is safe to
// render through fmt: the ID is only reachable through CurrentID. Attribute
// values are what the view exists to expose to the handler, so they are not
// secrets against the view itself.
func TestCurrentViewNeverRendersTheSessionID(t *testing.T) {
	ctx, _ := newTestContext(t, nil)
	setTestPrincipal(t, ctx, "alice", map[string]any{
		SessionIDAttribute: sensitiveSessionID,
		"secret_attribute": sensitiveAttribute,
	})
	view, ok := Current(ctx)
	if !ok {
		t.Fatal("Current() ok = false")
	}
	rendered := fmt.Sprintf("%+v %v %#v", view, view, view)
	assertNoSensitiveText(t, "current view rendering", rendered, sensitiveSessionID)
}
