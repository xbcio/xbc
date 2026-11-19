package session

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication"
	sessioncore "github.com/xbcio/xbc/extensions/authentication/session"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

func newTestContext(t *testing.T, cookies []*http.Cookie) (*web.Ctx, *httptest.ResponseRecorder) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	return enginetest.NewCtx(recorder, request), recorder
}

func setTestPrincipal(t *testing.T, ctx *web.Ctx, subject string, attributes map[string]any) {
	t.Helper()
	if !web.SetPrincipal(ctx, authentication.Principal{
		Subject:    subject,
		AuthMethod: string(sessioncore.Scheme),
		Attributes: attributes,
	}) {
		t.Fatal("web.SetPrincipal() = false")
	}
}

func TestLoginCreatesASessionAndWritesTheConfiguredCookie(t *testing.T) {
	p, core := configuredSessionPlugin(t, func(config *sessioncore.Config) {
		config.Name = "sid"
		config.SameSite = "strict"
	})
	ctx, recorder := newTestContext(t, nil)

	value, err := p.Login(context.Background(), ctx, "alice", map[string]any{"role": "admin"})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if value.Subject != "alice" || value.Attributes["role"] != "admin" {
		t.Fatalf("session = %+v, want subject alice with role admin", value)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %#v", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != "sid" || cookie.Value != value.ID || !cookie.HttpOnly || !cookie.Secure ||
		cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie = %#v", cookie)
	}
	if _, verdict, err := core.Verify(context.Background(), value.ID); err != nil || verdict != sessioncore.VerdictValid {
		t.Fatalf("Verify(new session) = %v, %v; want valid", verdict, err)
	}
}

func TestLoginRevokesAValidPresentedSession(t *testing.T) {
	p, core := configuredSessionPlugin(t, nil)
	previous, err := core.Create(context.Background(), "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, recorder := newTestContext(t, []*http.Cookie{{Name: defaultCookieName, Value: previous.ID}})

	value, err := p.Login(context.Background(), ctx, "alice", nil)
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if value.ID == previous.ID {
		t.Fatal("Login() reused the presented session ID")
	}
	if _, verdict, err := core.Verify(context.Background(), previous.ID); err != nil || verdict != sessioncore.VerdictNotFound {
		t.Fatalf("Verify(old session) = %v, %v; want not found", verdict, err)
	}
	if _, verdict, err := core.Verify(context.Background(), value.ID); err != nil || verdict != sessioncore.VerdictValid {
		t.Fatalf("Verify(new session) = %v, %v; want valid", verdict, err)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != value.ID {
		t.Fatalf("cookies = %#v, want the new session ID", cookies)
	}
}

func TestLoginProceedsWhenThePresentedSessionIsMalformedOrUnknown(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"malformed": "not-an-opaque-id",
		"unknown":   testID(7, 32),
	}
	for name, presented := range tests {
		presented := presented
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, core := configuredSessionPlugin(t, nil)
			ctx, _ := newTestContext(t, []*http.Cookie{{Name: defaultCookieName, Value: presented}})
			value, err := p.Login(context.Background(), ctx, "alice", nil)
			if err != nil {
				t.Fatalf("Login() error = %v", err)
			}
			if value.ID == presented {
				t.Fatal("Login() reused a dead presented ID")
			}
			if _, verdict, err := core.Verify(context.Background(), value.ID); err != nil || verdict != sessioncore.VerdictValid {
				t.Fatalf("Verify(new session) = %v, %v; want valid", verdict, err)
			}
		})
	}
}

func TestLoginRollsBackWhenTheWritableCarrierFails(t *testing.T) {
	_, core := configuredSessionPlugin(t, nil)
	carrier := &fixedCarrier{
		result:   authentication.Absent(),
		writable: true,
		issueErr: errors.New("carrier write failed"),
	}
	adapter, err := New(core, WithCarriers(carrier))
	if err != nil {
		t.Fatalf("New(WithCarriers) error = %v", err)
	}
	ctx, recorder := newTestContext(t, nil)

	value, err := adapter.Login(context.Background(), ctx, "alice", map[string]any{"role": "admin"})
	if err == nil {
		t.Fatalf("Login() = %+v, nil; want an error", value)
	}
	if len(carrier.issued) != 1 {
		t.Fatalf("carrier saw %d sessions, want 1", len(carrier.issued))
	}
	if len(recorder.Result().Cookies()) != 0 {
		t.Fatalf("failed login wrote cookies: %#v", recorder.Result().Cookies())
	}
	if _, verdict, verifyErr := core.Verify(context.Background(), carrier.issued[0].ID); verifyErr != nil || verdict != sessioncore.VerdictNotFound {
		t.Fatalf("Verify(rolled back session) = %v, %v; want not found", verdict, verifyErr)
	}
}

func TestLoginOverAReadOnlyChainReportsNoWritableCarrierAndCreatesNothing(t *testing.T) {
	_, core := configuredSessionPlugin(t, nil)
	header, err := NewHeaderCarrier(DefaultHeaderCarrierConfig())
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(core, WithCarriers(header))
	if err != nil {
		t.Fatalf("New(WithCarriers) error = %v", err)
	}
	ctx, recorder := newTestContext(t, nil)

	if _, err := adapter.Login(context.Background(), ctx, "alice", nil); !errors.Is(err, ErrNoWritableCarrier) {
		t.Fatalf("Login() = %v, want ErrNoWritableCarrier", err)
	}
	if len(recorder.Result().Cookies()) != 0 {
		t.Fatalf("read-only login wrote cookies: %#v", recorder.Result().Cookies())
	}
	sessions, err := core.ListBySubject(context.Background(), "alice")
	if err != nil {
		t.Fatalf("ListBySubject() error = %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("sessions = %+v, want none", sessions)
	}
}

func TestLoginReturnsRejectNewWithoutWriting(t *testing.T) {
	p, core := configuredSessionPlugin(t, func(config *sessioncore.Config) {
		config.MaxSessions = 1
		config.OnExceed = sessioncore.OnExceedRejectNew
	})
	if _, err := core.Create(context.Background(), "alice", nil); err != nil {
		t.Fatal(err)
	}
	ctx, recorder := newTestContext(t, nil)

	value, err := p.Login(context.Background(), ctx, "alice", nil)
	if err != sessioncore.ErrTooManySessions {
		t.Fatalf("Login() = %+v, %v; want the untouched ErrTooManySessions", value, err)
	}
	if len(recorder.Result().Cookies()) != 0 {
		t.Fatalf("rejected login wrote cookies: %#v", recorder.Result().Cookies())
	}
	sessions, err := core.ListBySubject(context.Background(), "alice")
	if err != nil {
		t.Fatalf("ListBySubject() error = %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %+v, want the original session alone", sessions)
	}
}

func TestLogoutRevokesAndClearsTheCookie(t *testing.T) {
	p, core := configuredSessionPlugin(t, nil)
	value, err := core.Create(context.Background(), "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, recorder := newTestContext(t, []*http.Cookie{{Name: defaultCookieName, Value: value.ID}})

	if err := p.Logout(context.Background(), ctx); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	if _, verdict, err := core.Verify(context.Background(), value.ID); err != nil || verdict != sessioncore.VerdictNotFound {
		t.Fatalf("Verify(revoked session) = %v, %v; want not found", verdict, err)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != defaultCookieName || cookies[0].Value != "" || cookies[0].MaxAge != -1 {
		t.Fatalf("clear cookie = %#v", cookies)
	}
}

func TestLogoutIsIdempotentForAbsentMalformedAndUnknownSessions(t *testing.T) {
	t.Parallel()

	tests := map[string][]*http.Cookie{
		"absent":    nil,
		"malformed": {{Name: defaultCookieName, Value: "!!"}},
		"unknown":   {{Name: defaultCookieName, Value: testID(8, 32)}},
	}
	for name, cookies := range tests {
		cookies := cookies
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, _ := configuredSessionPlugin(t, nil)
			ctx, recorder := newTestContext(t, cookies)
			if err := p.Logout(context.Background(), ctx); err != nil {
				t.Fatalf("Logout() error = %v", err)
			}
			if len(recorder.Result().Cookies()) != 1 {
				t.Fatalf("cookies = %#v, want one clear cookie", recorder.Result().Cookies())
			}
		})
	}
}

func TestLogoutUsesTheAuthenticatedPrincipalWhenTheRequestCarriesNoCookie(t *testing.T) {
	p, core := configuredSessionPlugin(t, nil)
	value, err := core.Create(context.Background(), "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ := newTestContext(t, nil)
	setTestPrincipal(t, ctx, "alice", map[string]any{SessionIDAttribute: value.ID})

	if err := p.Logout(context.Background(), ctx); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	if _, verdict, err := core.Verify(context.Background(), value.ID); err != nil || verdict != sessioncore.VerdictNotFound {
		t.Fatalf("Verify(revoked session) = %v, %v; want not found", verdict, err)
	}
}

func TestLogoutRevokesAHeaderChainSessionWithoutWriting(t *testing.T) {
	_, core := configuredSessionPlugin(t, nil)
	header, err := NewHeaderCarrier(DefaultHeaderCarrierConfig())
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(core, WithCarriers(header))
	if err != nil {
		t.Fatalf("New(WithCarriers) error = %v", err)
	}
	value, err := core.Create(context.Background(), "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, recorder := newTestContextWithHeaders(t, map[string][]string{
		"Authorization": {"Session " + value.ID},
	})

	if err := adapter.Logout(context.Background(), ctx); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	if _, verdict, err := core.Verify(context.Background(), value.ID); err != nil || verdict != sessioncore.VerdictNotFound {
		t.Fatalf("Verify(revoked session) = %v, %v; want not found", verdict, err)
	}
	if len(recorder.Result().Cookies()) != 0 {
		t.Fatalf("header-only logout wrote cookies: %#v", recorder.Result().Cookies())
	}
}

func TestRefreshAndPatchUpdateTheSessionVisibleToLaterRequests(t *testing.T) {
	p, core := configuredSessionPlugin(t, nil)
	value, err := core.Create(context.Background(), "alice", map[string]any{"role": "admin"})
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: defaultCookieName, Value: value.ID}

	refreshContext, _ := newTestContext(t, []*http.Cookie{cookie})
	refreshed, err := p.Refresh(context.Background(), refreshContext, map[string]any{"role": "viewer"})
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if refreshed.Attributes["role"] != "viewer" {
		t.Fatalf("refreshed attributes = %#v", refreshed.Attributes)
	}
	verified, verdict, err := core.Verify(context.Background(), value.ID)
	if err != nil || verdict != sessioncore.VerdictValid || verified.Attributes["role"] != "viewer" {
		t.Fatalf("Verify after Refresh() = %+v, %v, %v", verified, verdict, err)
	}

	patchContext, _ := newTestContext(t, []*http.Cookie{cookie})
	patched, err := p.Patch(context.Background(), patchContext, map[string]any{"team": "red"}, []string{"role"})
	if err != nil {
		t.Fatalf("Patch() error = %v", err)
	}
	if _, ok := patched.Attributes["role"]; ok || patched.Attributes["team"] != "red" {
		t.Fatalf("patched attributes = %#v, want role removed and team red", patched.Attributes)
	}
	verified, verdict, err = core.Verify(context.Background(), value.ID)
	if err != nil || verdict != sessioncore.VerdictValid || verified.Attributes["team"] != "red" {
		t.Fatalf("Verify after Patch() = %+v, %v, %v", verified, verdict, err)
	}
}

func TestRefreshAndPatchResolveTheSessionFromTheCarrierChain(t *testing.T) {
	_, core := configuredSessionPlugin(t, nil)
	cookieCarrier, err := NewCookieCarrier(core.Cookie())
	if err != nil {
		t.Fatal(err)
	}
	header, err := NewHeaderCarrier(DefaultHeaderCarrierConfig())
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(core, WithCarriers(cookieCarrier, header))
	if err != nil {
		t.Fatalf("New(WithCarriers) error = %v", err)
	}
	value, err := core.Create(context.Background(), "alice", map[string]any{"role": "admin"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ := newTestContextWithHeaders(t, map[string][]string{
		"Authorization": {"Session " + value.ID},
	})

	refreshed, err := adapter.Refresh(context.Background(), ctx, map[string]any{"role": "viewer"})
	if err != nil {
		t.Fatalf("Refresh(header carrier) error = %v", err)
	}
	if refreshed.Attributes["role"] != "viewer" {
		t.Fatalf("refreshed attributes = %#v", refreshed.Attributes)
	}
}

func TestRefreshAndPatchWithoutACurrentSessionReportNoCurrentSession(t *testing.T) {
	p, _ := configuredSessionPlugin(t, nil)
	ctx, _ := newTestContext(t, nil)

	if _, err := p.Refresh(context.Background(), ctx, nil); !errors.Is(err, ErrNoCurrentSession) {
		t.Fatalf("Refresh() = %v, want ErrNoCurrentSession", err)
	}
	if _, err := p.Patch(context.Background(), ctx, nil, nil); !errors.Is(err, ErrNoCurrentSession) {
		t.Fatalf("Patch() = %v, want ErrNoCurrentSession", err)
	}
}

func TestCurrentReturnsACopiedViewWithoutTheSessionID(t *testing.T) {
	ctx, _ := newTestContext(t, nil)
	setTestPrincipal(t, ctx, "alice", map[string]any{
		SessionIDAttribute: "opaque-id",
		"role":             "admin",
	})

	view, ok := Current(ctx)
	if !ok {
		t.Fatal("Current() ok = false")
	}
	if view.Subject != "alice" || view.Attributes["role"] != "admin" {
		t.Fatalf("view = %+v, want subject alice and role admin", view)
	}
	if _, present := view.Attributes[SessionIDAttribute]; present {
		t.Fatalf("view attributes = %#v, want session_id removed", view.Attributes)
	}
	view.Attributes["role"] = "tampered"
	again, ok := Current(ctx)
	if !ok || again.Attributes["role"] != "admin" {
		t.Fatalf("second Current() = %+v, ok=%v; want the original attributes", again, ok)
	}

	id, ok := CurrentID(ctx)
	if !ok || id != "opaque-id" {
		t.Fatalf("CurrentID() = %q, %v; want opaque-id, true", id, ok)
	}
}

func TestCurrentReportsNoPrincipalWithoutASession(t *testing.T) {
	plain, _ := newTestContext(t, nil)
	if _, ok := Current(plain); ok {
		t.Fatal("Current() = true without a principal")
	}
	if _, ok := CurrentID(plain); ok {
		t.Fatal("CurrentID() = true without a principal")
	}
	if _, ok := Current(nil); ok {
		t.Fatal("Current(nil) = true")
	}
}
