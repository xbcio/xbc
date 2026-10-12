package session

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication"
	sessioncore "github.com/xbcio/xbc/extensions/authentication/session"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

// configuredSessionPlugin returns an adapter over a memory-backed neutral
// session plugin whose configuration is the default plus mutate.
func configuredSessionPlugin(t *testing.T, mutate func(*sessioncore.Config)) (*Plugin, *sessioncore.Plugin) {
	t.Helper()
	cfg := sessioncore.DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	core, err := sessioncore.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Stop(context.Background()) })
	p, err := New(core)
	if err != nil {
		t.Fatal(err)
	}
	return p, core
}

// testID returns a well-formed opaque session ID of size random bytes.
func testID(seed byte, size int) string {
	raw := make([]byte, size)
	for i := range raw {
		raw[i] = seed + byte(i)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

const defaultCookieName = "xbc_session"

// newTestContextWithCookies builds a *web.Ctx wrapping a bare request
// carrying the given cookies. A nil slice means the request has no cookies
// at all -- the minimum ExtractCredential needs to classify a request.
func newTestContextWithCookies(t *testing.T, cookies []*http.Cookie) *web.Ctx {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	return enginetest.NewCtx(httptest.NewRecorder(), req)
}

// TestExtractCredentialClassifiesCookie exercises the three-state
// extraction boundary: cookie absent, cookie present but no value can be read
// from it (empty value, or more than one cookie sharing the configured
// name), and cookie present with a value -- Presented regardless of whether
// that value looks like a well-formed session ID, because shape validation
// is Authenticate's job, not the extractor's.
//
// Every subtest runs against both the default cookie name and a
// non-default configured one: asserting only against DefaultConfig() would
// leave a hardcoded "xbc_session" literal over Config.Name entirely
// unnoticed by this suite.
func TestExtractCredentialClassifiesCookie(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		cookies    func(configuredName string) []*http.Cookie
		want       authentication.CredentialStatus
		wantReason authentication.SafeReason
	}{
		{
			name:    "no cookie at all is absent",
			cookies: func(string) []*http.Cookie { return nil },
			want:    authentication.CredentialStatusAbsent,
		},
		{
			name: "a cookie under a different name is absent",
			cookies: func(string) []*http.Cookie {
				return []*http.Cookie{{Name: "unrelated_cookie", Value: "abc"}}
			},
			want: authentication.CredentialStatusAbsent,
		},
		{
			name: "configured cookie with an empty value is malformed",
			cookies: func(configuredName string) []*http.Cookie {
				return []*http.Cookie{{Name: configuredName, Value: ""}}
			},
			want:       authentication.CredentialStatusMalformed,
			wantReason: "malformed credential",
		},
		{
			// Two cookies sharing the configured name: the extractor cannot pick
			// a value, so this is Malformed, not Absent and not a silent
			// first-wins.
			name: "duplicate cookies under the configured name are malformed",
			cookies: func(configuredName string) []*http.Cookie {
				return []*http.Cookie{
					{Name: configuredName, Value: "aaa"},
					{Name: configuredName, Value: "bbb"},
				}
			},
			want:       authentication.CredentialStatusMalformed,
			wantReason: "malformed credential",
		},
		{
			// A value that does not look like a well-formed opaque session ID is
			// still Presented: validID is Authenticate's check, not the
			// extractor's.
			name: "configured cookie with a non-ID-shaped value is presented",
			cookies: func(configuredName string) []*http.Cookie {
				return []*http.Cookie{{Name: configuredName, Value: "not-an-opaque-id"}}
			},
			want: authentication.CredentialStatusPresented,
		},
		{
			name: "configured cookie with a well-formed session ID is presented",
			cookies: func(configuredName string) []*http.Cookie {
				return []*http.Cookie{{Name: configuredName, Value: testID(1, 32)}}
			},
			want: authentication.CredentialStatusPresented,
		},
	}

	for _, configuredName := range []string{defaultCookieName, "custom_session_cookie"} {
		configuredName := configuredName
		for _, test := range tests {
			test := test
			t.Run(configuredName+"/"+test.name, func(t *testing.T) {
				t.Parallel()
				p, _ := configuredSessionPlugin(t, func(cfg *sessioncore.Config) { cfg.Name = configuredName })
				cookies := test.cookies(configuredName)
				c := newTestContextWithCookies(t, cookies)

				got, err := p.ExtractCredential(c)
				if err != nil {
					t.Fatalf("ExtractCredential() error = %v", err)
				}
				if got.Status() != test.want {
					t.Fatalf("status = %v, want %v", got.Status(), test.want)
				}
				// Ruling 17: session never advertises a WWW-Authenticate challenge,
				// unlike jwt and apikey. Asserting only Status() would not catch a
				// future edit that "fixes" this for symmetry by switching to
				// AbsentWithChallenge/MalformedWithChallenge.
				if challenge, ok := got.Challenge(); ok {
					t.Fatalf("Challenge() = %q, want none", challenge)
				}
				switch test.want {
				case authentication.CredentialStatusMalformed:
					reason, ok := got.Reason()
					if !ok || reason != test.wantReason {
						t.Fatalf("Reason() = %q, ok=%v, want %q", reason, ok, test.wantReason)
					}
					if strings.Contains(string(reason), configuredName) {
						t.Fatalf("reason leaked the configured cookie name: %q", reason)
					}
				case authentication.CredentialStatusPresented:
					credential, ok := got.Credential()
					if !ok {
						t.Fatal("Credential() ok = false")
					}
					value, ok := credential.Value().(string)
					if !ok || value != cookies[0].Value {
						t.Fatalf("credential value = %#v, want %q (the raw cookie value)", credential.Value(), cookies[0].Value)
					}
				}
			})
		}
	}
}

// TestExtractCredentialWithoutRequestIsAbsent exercises both halves of
// ExtractCredential's defensive guard: a nil *web.Ctx, and a *web.Ctx whose
// request context carries no request. Neither web.NewCtx nor the test engine
// validates that, so the second case is reachable from outside the web
// package; a Ctx the framework itself hands a handler always wraps a live
// request.
func TestExtractCredentialWithoutRequestIsAbsent(t *testing.T) {
	t.Parallel()
	p, _ := configuredSessionPlugin(t, nil)

	for name, c := range map[string]*web.Ctx{
		"nil ctx":     nil,
		"nil request": enginetest.NewCtx(httptest.NewRecorder(), nil),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := p.ExtractCredential(c)
			if err != nil {
				t.Fatalf("ExtractCredential() error = %v", err)
			}
			if got.Status() != authentication.CredentialStatusAbsent {
				t.Fatalf("status = %v, want Absent", got.Status())
			}
			if challenge, ok := got.Challenge(); ok {
				t.Fatalf("Challenge() = %q, want none", challenge)
			}
		})
	}
}

// TestExtractCredentialFeedsAuthenticateWithTheExactCookieValue is the only
// test that puts a cookie on a request, runs it through ExtractCredential, and
// feeds the resulting Credential straight into the neutral Authenticate. Every
// Authenticate test in the neutral package builds its Credential by hand with
// authentication.Presented(id), so none of them would notice ExtractCredential
// returning the wrong field -- this is the one connecting "the value in the
// cookie" to "the value that reaches Authenticate".
func TestExtractCredentialFeedsAuthenticateWithTheExactCookieValue(t *testing.T) {
	p, core := configuredSessionPlugin(t, nil)
	created, err := p.Create(context.Background(), "carol", nil)
	if err != nil {
		t.Fatal(err)
	}
	id := created.ID
	c := newTestContextWithCookies(t, []*http.Cookie{{Name: defaultCookieName, Value: id}})

	extracted, err := p.ExtractCredential(c)
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
	if !result.Authenticated() {
		t.Fatal("Authenticated() = false, want true")
	}
	principal, ok := result.Principal()
	if !ok {
		t.Fatal("Principal() ok = false")
	}
	typed, ok := principal.(authentication.Principal)
	if !ok {
		t.Fatalf("principal type = %T, want authentication.Principal", principal)
	}
	if typed.Subject != "carol" || typed.Attributes["session_id"] != id {
		t.Fatalf("principal = %#v, want subject %q and session_id %q", typed, "carol", id)
	}
}
