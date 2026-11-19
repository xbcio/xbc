package session

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xbcio/xbc/extensions/authentication"
	sessioncore "github.com/xbcio/xbc/extensions/authentication/session"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

// fixedCarrier is a test carrier with a constant extraction result and a
// controllable write outcome.
type fixedCarrier struct {
	result   authentication.CredentialResult
	writable bool
	issueErr error
	issued   []sessioncore.Session
	cleared  int
}

func (carrier *fixedCarrier) Extract(*web.Ctx) authentication.CredentialResult {
	return carrier.result
}

func (carrier *fixedCarrier) Writable() bool { return carrier.writable }

func (carrier *fixedCarrier) Issue(_ *web.Ctx, value sessioncore.Session) error {
	carrier.issued = append(carrier.issued, value)
	return carrier.issueErr
}

func (carrier *fixedCarrier) Clear(*web.Ctx) { carrier.cleared++ }

func newTestContextWithHeaders(t *testing.T, headers map[string][]string) (*web.Ctx, *httptest.ResponseRecorder) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	recorder := httptest.NewRecorder()
	return enginetest.NewCtx(recorder, request), recorder
}

func TestCookieCarrierClassifiesRequests(t *testing.T) {
	t.Parallel()

	settings := sessioncore.CookieSettings{
		Name:     "sid",
		Path:     "/",
		HTTPOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
	carrier, err := NewCookieCarrier(settings)
	if err != nil {
		t.Fatalf("NewCookieCarrier() error = %v", err)
	}

	tests := []struct {
		name    string
		cookies []*http.Cookie
		want    authentication.CredentialStatus
		value   string
	}{
		{name: "absent", want: authentication.CredentialStatusAbsent},
		{
			name:    "other cookie name is absent",
			cookies: []*http.Cookie{{Name: "other", Value: "abc"}},
			want:    authentication.CredentialStatusAbsent,
		},
		{
			name:    "empty value is malformed",
			cookies: []*http.Cookie{{Name: "sid", Value: ""}},
			want:    authentication.CredentialStatusMalformed,
		},
		{
			name: "duplicate cookies are malformed",
			cookies: []*http.Cookie{
				{Name: "sid", Value: "one"},
				{Name: "sid", Value: "two"},
			},
			want: authentication.CredentialStatusMalformed,
		},
		{
			name:    "single value is presented",
			cookies: []*http.Cookie{{Name: "sid", Value: "opaque-id"}},
			want:    authentication.CredentialStatusPresented,
			value:   "opaque-id",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := newTestContextWithCookies(t, test.cookies)
			result := carrier.Extract(ctx)
			if result.Status() != test.want {
				t.Fatalf("status = %v, want %v", result.Status(), test.want)
			}
			if test.want == authentication.CredentialStatusPresented {
				credential, ok := result.Credential()
				if !ok || credential.Value() != test.value {
					t.Fatalf("credential = %#v, ok=%v; want %q", credential.Value(), ok, test.value)
				}
			}
		})
	}
}

func TestHeaderCarrierClassifiesRequests(t *testing.T) {
	t.Parallel()

	carrier, err := NewHeaderCarrier(DefaultHeaderCarrierConfig())
	if err != nil {
		t.Fatalf("NewHeaderCarrier() error = %v", err)
	}

	tests := []struct {
		name    string
		headers map[string][]string
		want    authentication.CredentialStatus
		value   string
	}{
		{name: "absent", want: authentication.CredentialStatusAbsent},
		{
			name:    "other header is absent",
			headers: map[string][]string{"X-Session": {"Session abc"}},
			want:    authentication.CredentialStatusAbsent,
		},
		{
			name:    "wrong scheme is malformed",
			headers: map[string][]string{"Authorization": {"Bearer abc"}},
			want:    authentication.CredentialStatusMalformed,
		},
		{
			name:    "empty value after the scheme is malformed",
			headers: map[string][]string{"Authorization": {"Session   "}},
			want:    authentication.CredentialStatusMalformed,
		},
		{
			name:    "duplicate headers are malformed",
			headers: map[string][]string{"Authorization": {"Session one", "Session two"}},
			want:    authentication.CredentialStatusMalformed,
		},
		{
			name:    "scheme comparison is case-insensitive",
			headers: map[string][]string{"Authorization": {"session abc"}},
			want:    authentication.CredentialStatusPresented,
			value:   "abc",
		},
		{
			name:    "presented",
			headers: map[string][]string{"Authorization": {"Session abc"}},
			want:    authentication.CredentialStatusPresented,
			value:   "abc",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx, _ := newTestContextWithHeaders(t, test.headers)
			result := carrier.Extract(ctx)
			if result.Status() != test.want {
				t.Fatalf("status = %v, want %v", result.Status(), test.want)
			}
			if test.want == authentication.CredentialStatusPresented {
				credential, ok := result.Credential()
				if !ok || credential.Value() != test.value {
					t.Fatalf("credential = %#v, ok=%v; want %q", credential.Value(), ok, test.value)
				}
			}
		})
	}
}

func TestCarrierChainSelectsTheFirstPresentedValueAndFailsClosed(t *testing.T) {
	t.Parallel()

	cookie := func(status authentication.CredentialStatus, value string) Carrier {
		result := authentication.Absent()
		switch status {
		case authentication.CredentialStatusPresented:
			result = authentication.Presented(value)
		case authentication.CredentialStatusMalformed:
			result = authentication.Malformed(authentication.ReasonMalformedCredential)
		}
		return &fixedCarrier{result: result}
	}

	tests := []struct {
		name     string
		carriers []Carrier
		want     authentication.CredentialStatus
		value    string
	}{
		{
			name: "cookie first wins",
			carriers: []Carrier{
				cookie(authentication.CredentialStatusPresented, "cookie-value"),
				cookie(authentication.CredentialStatusPresented, "header-value"),
			},
			want:  authentication.CredentialStatusPresented,
			value: "cookie-value",
		},
		{
			name: "header first wins when earlier source is absent",
			carriers: []Carrier{
				cookie(authentication.CredentialStatusAbsent, ""),
				cookie(authentication.CredentialStatusPresented, "header-value"),
			},
			want:  authentication.CredentialStatusPresented,
			value: "header-value",
		},
		{
			name: "a later malformed source fails the whole chain",
			carriers: []Carrier{
				cookie(authentication.CredentialStatusPresented, "cookie-value"),
				cookie(authentication.CredentialStatusMalformed, ""),
			},
			want: authentication.CredentialStatusMalformed,
		},
		{
			name: "an earlier malformed source fails the whole chain",
			carriers: []Carrier{
				cookie(authentication.CredentialStatusMalformed, ""),
				cookie(authentication.CredentialStatusPresented, "header-value"),
			},
			want: authentication.CredentialStatusMalformed,
		},
		{
			name:     "all absent is absent",
			carriers: []Carrier{cookie(authentication.CredentialStatusAbsent, ""), cookie(authentication.CredentialStatusAbsent, "")},
			want:     authentication.CredentialStatusAbsent,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			chain, err := newCarrierChain(test.carriers)
			if err != nil {
				t.Fatalf("newCarrierChain() error = %v", err)
			}
			result := chain.Extract(newTestContextWithCookies(t, nil))
			if result.Status() != test.want {
				t.Fatalf("status = %v, want %v", result.Status(), test.want)
			}
			if test.want == authentication.CredentialStatusPresented {
				credential, ok := result.Credential()
				if !ok || credential.Value() != test.value {
					t.Fatalf("credential = %#v, ok=%v; want %q", credential.Value(), ok, test.value)
				}
			}
			if challenge, ok := result.Challenge(); ok {
				t.Fatalf("Challenge() = %q, want none", challenge)
			}
		})
	}
}

func TestCarrierChainRejectsInvalidCompositions(t *testing.T) {
	t.Parallel()

	if _, err := newCarrierChain(nil); err == nil {
		t.Fatal("newCarrierChain(nil) = nil error")
	}
	if _, err := newCarrierChain([]Carrier{nil}); err == nil {
		t.Fatal("newCarrierChain(nil entry) = nil error")
	}
	first := &fixedCarrier{result: authentication.Absent(), writable: true}
	second := &fixedCarrier{result: authentication.Absent(), writable: true}
	if _, err := newCarrierChain([]Carrier{first, second}); err == nil {
		t.Fatal("newCarrierChain(two writers) = nil error")
	}
	readOnly := []Carrier{
		&fixedCarrier{result: authentication.Absent()},
		&fixedCarrier{result: authentication.Absent()},
	}
	chain, err := newCarrierChain(readOnly)
	if err != nil {
		t.Fatalf("newCarrierChain(read-only chain) error = %v", err)
	}
	if chain.writer != nil {
		t.Fatalf("writer = %#v, want nil", chain.writer)
	}
}

func TestCookieCarrierIssueAppliesSettingsAndClearExpiresTheCookie(t *testing.T) {
	t.Parallel()

	settings := sessioncore.CookieSettings{
		Name:     "__Host-sid",
		Path:     "/",
		HTTPOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	}
	carrier, err := NewCookieCarrier(settings)
	if err != nil {
		t.Fatalf("NewCookieCarrier() error = %v", err)
	}
	value := sessioncore.Session{ID: testID(3, 32), ExpiresAt: time.Now().Add(time.Hour)}
	recorder := httptest.NewRecorder()
	ctx := enginetest.NewCtx(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if err := carrier.Issue(ctx, value); err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %#v", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != settings.Name || cookie.Value != value.ID || cookie.Path != "/" ||
		!cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie = %#v", cookie)
	}
	if cookie.MaxAge <= 0 || cookie.MaxAge > 3600 {
		t.Fatalf("MaxAge = %d, want about one hour", cookie.MaxAge)
	}

	cleared := httptest.NewRecorder()
	carrier.Clear(enginetest.NewCtx(cleared, httptest.NewRequest(http.MethodGet, "/", nil)))
	clearCookies := cleared.Result().Cookies()
	if len(clearCookies) != 1 || clearCookies[0].Name != settings.Name ||
		clearCookies[0].Value != "" || clearCookies[0].MaxAge != -1 ||
		!clearCookies[0].HttpOnly || !clearCookies[0].Secure {
		t.Fatalf("clear cookie = %#v", clearCookies)
	}
}

func TestCookieCarrierRefusesInvalidSessionsAndMissingWriter(t *testing.T) {
	t.Parallel()

	carrier, err := NewCookieCarrier(sessioncore.CookieSettings{
		Name:     "sid",
		Path:     "/",
		HTTPOnly: true,
		Secure:   true,
	})
	if err != nil {
		t.Fatalf("NewCookieCarrier() error = %v", err)
	}
	ctx := enginetest.NewCtx(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if err := carrier.Issue(ctx, sessioncore.Session{ID: "", ExpiresAt: time.Now().Add(time.Hour)}); !errors.Is(err, sessioncore.ErrInvalidCookie) {
		t.Fatalf("Issue(empty ID) = %v, want ErrInvalidCookie", err)
	}
	if err := carrier.Issue(ctx, sessioncore.Session{ID: "abc", ExpiresAt: time.Now().Add(-time.Minute)}); !errors.Is(err, sessioncore.ErrInvalidCookie) {
		t.Fatalf("Issue(expired) = %v, want ErrInvalidCookie", err)
	}
	if err := carrier.Issue(nil, sessioncore.Session{ID: "abc", ExpiresAt: time.Now().Add(time.Hour)}); err == nil {
		t.Fatal("Issue(nil ctx) = nil error")
	}
	carrier.Clear(nil)
}

func TestHeaderCarrierIsReadOnly(t *testing.T) {
	t.Parallel()

	carrier, err := NewHeaderCarrier(DefaultHeaderCarrierConfig())
	if err != nil {
		t.Fatalf("NewHeaderCarrier() error = %v", err)
	}
	if carrier.Writable() {
		t.Fatal("Writable() = true, want false")
	}
	errorValue := carrier.Issue(nil, sessioncore.Session{ID: "abc"})
	if !errors.Is(errorValue, ErrNotWritable) {
		t.Fatalf("Issue() = %v, want ErrNotWritable", errorValue)
	}
	if strings.Contains(errorValue.Error(), "abc") {
		t.Fatalf("Issue() error leaked the session ID: %q", errorValue)
	}
	recorder := httptest.NewRecorder()
	carrier.Clear(enginetest.NewCtx(recorder, httptest.NewRequest(http.MethodGet, "/", nil)))
	if len(recorder.Result().Cookies()) != 0 {
		t.Fatalf("Clear() wrote cookies: %#v", recorder.Result().Cookies())
	}
}

func TestNewCookieCarrierValidatesSettings(t *testing.T) {
	t.Parallel()

	tests := map[string]sessioncore.CookieSettings{
		"invalid name":          {Name: "bad name", Path: "/", HTTPOnly: true},
		"relative path":         {Name: "sid", Path: "relative", HTTPOnly: true},
		"unsafe domain":         {Name: "sid", Path: "/", Domain: "bad/domain", HTTPOnly: true},
		"script-readable":       {Name: "sid", Path: "/", HTTPOnly: false},
		"none without secure":   {Name: "sid", Path: "/", HTTPOnly: true, SameSite: http.SameSiteNoneMode},
		"host prefix without /": {Name: "__Host-sid", Path: "/sub", HTTPOnly: true, Secure: true},
	}
	for name, settings := range tests {
		settings := settings
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if carrier, err := NewCookieCarrier(settings); err == nil || carrier != nil {
				t.Fatalf("NewCookieCarrier(%+v) = %#v, %v; want nil carrier and error", settings, carrier, err)
			}
		})
	}
}

func TestNewHeaderCarrierNormalizesAndValidatesConfig(t *testing.T) {
	t.Parallel()

	carrier, err := NewHeaderCarrier(HeaderCarrierConfig{})
	if err != nil {
		t.Fatalf("NewHeaderCarrier(zero) error = %v", err)
	}
	if carrier.config != DefaultHeaderCarrierConfig() {
		t.Fatalf("config = %+v, want %+v", carrier.config, DefaultHeaderCarrierConfig())
	}
	if _, err := NewHeaderCarrier(HeaderCarrierConfig{Header: "bad header"}); err == nil {
		t.Fatal("NewHeaderCarrier(invalid header) = nil error")
	}
	if _, err := NewHeaderCarrier(HeaderCarrierConfig{Scheme: "bad\nscheme"}); err == nil {
		t.Fatal("NewHeaderCarrier(invalid scheme) = nil error")
	}
}
