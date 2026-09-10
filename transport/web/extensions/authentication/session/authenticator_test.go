package session

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/authentication"
	"github.com/xbcio/xbc/transport/web"
)

func init() { gin.SetMode(gin.TestMode) }

// configuredSessionPlugin returns a plugin backed by store, with cfg mutated
// by mutate before construction.
func configuredSessionPlugin(t *testing.T, store Store, mutate func(*Config)) *Plugin {
	t.Helper()
	cfg := DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := New(cfg, WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop(context.Background()) })
	return p
}

// newTestContextWithCookies builds a bare *gin.Context carrying the given
// cookies on its request. A nil slice means the request has no cookies at
// all -- the minimum ExtractCredential needs to classify a request.
func newTestContextWithCookies(t *testing.T, cookies []*http.Cookie) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	c.Request = req
	return c
}

// TestPluginExtractCredentialClassifiesCookie exercises the three-state
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
func TestPluginExtractCredentialClassifiesCookie(t *testing.T) {
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
				p := configuredSessionPlugin(t, &recordingStore{}, func(cfg *Config) { cfg.Name = configuredName })
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

func TestPluginExtractCredentialNilRequestIsAbsent(t *testing.T) {
	t.Parallel()
	p := configuredSessionPlugin(t, &recordingStore{}, nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

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
}

// TestPluginExtractCredentialFeedsAuthenticateWithTheExactCookieValue is the
// only test that puts a cookie on a request, runs it through
// ExtractCredential, and feeds the resulting Credential straight into
// Authenticate. Every other Authenticate test builds its Credential by hand
// with authentication.Presented(id), so none of them would notice
// ExtractCredential returning the wrong field -- this is the one connecting
// "the value in the cookie" to "the value that reaches Authenticate".
func TestPluginExtractCredentialFeedsAuthenticateWithTheExactCookieValue(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	store := newControlledMemoryStore(t, &now)
	id := testID(20, 32)
	if err := store.Create(context.Background(), sessionValue(id, "carol", now, time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	p := configuredSessionPlugin(t, store, nil)
	c := newTestContextWithCookies(t, []*http.Cookie{{Name: defaultCookieName, Value: id}})

	extracted, err := p.ExtractCredential(c)
	if err != nil {
		t.Fatalf("ExtractCredential() error = %v", err)
	}
	credential, ok := extracted.Credential()
	if !ok {
		t.Fatal("Credential() ok = false")
	}

	result, err := p.Authenticate(context.Background(), credential)
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
	typed, ok := principal.(web.Principal)
	if !ok {
		t.Fatalf("principal type = %T, want web.Principal", principal)
	}
	if typed.Subject != "carol" || typed.Attributes["session_id"] != id {
		t.Fatalf("principal = %#v, want subject %q and session_id %q", typed, "carol", id)
	}
}

func presentedSessionCredential(t *testing.T, value string) authentication.Credential {
	t.Helper()
	credential, ok := authentication.Presented(value).Credential()
	if !ok {
		t.Fatal("Credential() ok = false")
	}
	return credential
}

func assertSessionRejected(t *testing.T, result authentication.Result, err error, wantReason authentication.SafeReason) {
	t.Helper()
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if !result.Rejected() {
		t.Fatal("Rejected() = false, want true")
	}
	reason, ok := result.Reason()
	if !ok || reason != wantReason {
		t.Fatalf("Reason() = %q, ok=%v, want %q", reason, ok, wantReason)
	}
	// Ruling 17: Authenticate's rejections never carry a challenge either --
	// there is no session scheme token to advertise.
	if challenges := result.Challenges(); len(challenges) != 0 {
		t.Fatalf("Challenges() = %v, want none", challenges)
	}
}

func TestPluginAuthenticateAcceptsValidSessionAndPublishesPrincipal(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	store := newControlledMemoryStore(t, &now)
	id := testID(7, 32)
	if err := store.Create(context.Background(), sessionValue(id, "alice", now, time.Hour), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	p := configuredSessionPlugin(t, store, nil)

	result, err := p.Authenticate(context.Background(), presentedSessionCredential(t, id))
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
	typed, ok := principal.(web.Principal)
	if !ok {
		t.Fatalf("principal type = %T, want web.Principal", principal)
	}
	if typed.Subject != "alice" || typed.AuthMethod != "session" ||
		typed.Attributes["role"] != "admin" || typed.Attributes["session_id"] != id {
		t.Fatalf("principal = %#v", typed)
	}

	// The published Attributes must be a defensive copy: mutating it must not
	// reach back into the store's retained value.
	typed.Attributes["nested"].(map[string]any)["team"] = "principal-mutated"
	again, found, err := store.Get(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("Get() error = %v found = %v", err, found)
	}
	if again.Attributes["nested"].(map[string]any)["team"] != "core" {
		t.Fatal("Authenticate leaked a mutable reference into the store")
	}
}

// TestPluginAuthenticateAcceptsSessionWithNilAttributes exercises the branch
// the success-path tests above never reach: every one of them stores a
// session carrying attributes, so Authenticate's "attributes == nil" fallback
// -- allocating a fresh map before setting session_id -- has no test of its
// own without this case.
func TestPluginAuthenticateAcceptsSessionWithNilAttributes(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	store := newControlledMemoryStore(t, &now)
	id := testID(21, 32)
	value := Session{ID: id, Subject: "dave", IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := store.Create(context.Background(), value, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	p := configuredSessionPlugin(t, store, nil)

	result, err := p.Authenticate(context.Background(), presentedSessionCredential(t, id))
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
	typed, ok := principal.(web.Principal)
	if !ok {
		t.Fatalf("principal type = %T, want web.Principal", principal)
	}
	if typed.Subject != "dave" || typed.Attributes["session_id"] != id || len(typed.Attributes) != 1 {
		t.Fatalf("principal = %#v, want only session_id set", typed)
	}
}

func TestPluginAuthenticateRejectsInvalidCredentialType(t *testing.T) {
	p := configuredSessionPlugin(t, &recordingStore{}, nil)
	credential, ok := authentication.Presented(42).Credential()
	if !ok {
		t.Fatal("Credential() ok = false")
	}
	result, err := p.Authenticate(context.Background(), credential)
	assertSessionRejected(t, result, err, reasonInvalidCredential)
}

// TestPluginAuthenticateCollapsesShapeStoreAndClosedReasons pins the
// collapse required by Ruling 9/16's sibling ruling for Authenticate: an
// ID that fails validID's shape check, an unknown ID, a store error, an
// expired session, and a stopped plugin must all be indistinguishable to the
// caller. Splitting any of these out would hand an unauthenticated caller an
// oracle for whether a specific session ID exists or existed.
func TestPluginAuthenticateCollapsesShapeStoreAndClosedReasons(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()

	t.Run("malformed session ID shape", func(t *testing.T) {
		p := configuredSessionPlugin(t, &recordingStore{}, nil)
		result, err := p.Authenticate(context.Background(), presentedSessionCredential(t, "not-an-opaque-id"))
		assertSessionRejected(t, result, err, reasonInvalidCredential)
	})

	t.Run("unknown session ID", func(t *testing.T) {
		store := newControlledMemoryStore(t, &now)
		p := configuredSessionPlugin(t, store, nil)
		result, err := p.Authenticate(context.Background(), presentedSessionCredential(t, testID(9, 32)))
		assertSessionRejected(t, result, err, reasonInvalidCredential)
	})

	t.Run("expired session", func(t *testing.T) {
		localNow := now
		store := newControlledMemoryStore(t, &localNow)
		id := testID(10, 32)
		if err := store.Create(context.Background(), sessionValue(id, "alice", localNow, time.Minute), time.Minute); err != nil {
			t.Fatal(err)
		}
		localNow = localNow.Add(2 * time.Minute)
		p := configuredSessionPlugin(t, store, nil)
		result, err := p.Authenticate(context.Background(), presentedSessionCredential(t, id))
		assertSessionRejected(t, result, err, reasonInvalidCredential)
	})

	t.Run("store error", func(t *testing.T) {
		store := &recordingStore{touch: func(context.Context, string, time.Duration, time.Duration) (Session, bool, error) {
			return Session{}, false, errors.New("backend details")
		}}
		p := configuredSessionPlugin(t, store, nil)
		result, err := p.Authenticate(context.Background(), presentedSessionCredential(t, testID(11, 32)))
		assertSessionRejected(t, result, err, reasonInvalidCredential)
	})

	t.Run("stopped plugin", func(t *testing.T) {
		store := &recordingStore{touch: func(context.Context, string, time.Duration, time.Duration) (Session, bool, error) {
			t.Fatal("Authenticate must reject a stopped plugin before touching the store")
			return Session{}, false, nil
		}}
		p := configuredSessionPlugin(t, store, nil)
		if err := p.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		result, err := p.Authenticate(context.Background(), presentedSessionCredential(t, testID(12, 32)))
		assertSessionRejected(t, result, err, reasonInvalidCredential)
	})
}

// TestPluginAuthenticateBoundsStoreOperationAndHidesBackendErrors proves that
// once the store call's context is canceled, Authenticate rejects rather than
// hanging or leaking the ctx error, and that whatever the backend reports
// never reaches the caller's reason. It does not pin the exact
// operation_timeout duration placed on that context -- any deadline satisfies
// the blocking store double below. See
// TestPluginAuthenticateAppliesConfiguredStoreParameters for that.
func TestPluginAuthenticateBoundsStoreOperationAndHidesBackendErrors(t *testing.T) {
	store := &recordingStore{touch: func(ctx context.Context, _ string, _, _ time.Duration) (Session, bool, error) {
		<-ctx.Done()
		return Session{}, false, ctx.Err()
	}}
	p := configuredSessionPlugin(t, store, func(cfg *Config) { cfg.OperationTimeout = time.Millisecond })
	result, err := p.Authenticate(context.Background(), presentedSessionCredential(t, testID(13, 32)))
	assertSessionRejected(t, result, err, reasonInvalidCredential)

	errorStore := &recordingStore{touch: func(context.Context, string, time.Duration, time.Duration) (Session, bool, error) {
		return Session{}, false, errors.New("backend details")
	}}
	errorPlugin := configuredSessionPlugin(t, errorStore, nil)
	result, err = errorPlugin.Authenticate(context.Background(), presentedSessionCredential(t, testID(14, 32)))
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	reason, _ := result.Reason()
	if strings.Contains(string(reason), "backend details") {
		t.Fatalf("backend details leaked: %q", reason)
	}
}

// TestPluginAuthenticateAppliesConfiguredStoreParameters pins three
// configurable knobs a naive refactor can silently break: the exact idle_ttl
// and touch_interval values -- in the documented order -- Authenticate
// passes to store.Touch, the operation_timeout duration actually left on the
// store call's context, and id_bytes' role in validating the credential
// before it ever reaches the store. The non-default case is what makes each
// assertion discriminating: id_bytes=64 rejects a well-formed 64-byte session
// ID the moment the shape check falls back to a hardcoded 32, a swapped
// ttl/interval pair shows up as a mismatched recording against either case,
// and a non-default operation_timeout means a hardcoded constant leaves the
// wrong amount of time on the context instead of coincidentally matching the
// default.
func TestPluginAuthenticateAppliesConfiguredStoreParameters(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		idBytes int
	}{
		{name: "default config", idBytes: 32},
		{
			name: "non-default id_bytes and operation_timeout",
			mutate: func(cfg *Config) {
				cfg.IDBytes = 64
				cfg.OperationTimeout = 750 * time.Millisecond
			},
			idBytes: 64,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cfg := DefaultConfig()
			if test.mutate != nil {
				test.mutate(&cfg)
			}
			normalized, err := normalizeConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}

			var (
				mu                           sync.Mutex
				gotIdleTTL, gotTouchInterval time.Duration
				gotRemaining                 time.Duration
				gotDeadlineOK                bool
			)
			store := &recordingStore{touch: func(ctx context.Context, id string, ttl, interval time.Duration) (Session, bool, error) {
				mu.Lock()
				defer mu.Unlock()
				gotIdleTTL, gotTouchInterval = ttl, interval
				deadline, ok := ctx.Deadline()
				gotDeadlineOK = ok
				if ok {
					gotRemaining = time.Until(deadline)
				}
				return Session{ID: id, Subject: "alice", Attributes: map[string]any{"role": "admin"}}, true, nil
			}}

			p := configuredSessionPlugin(t, store, test.mutate)
			id := testID(40, test.idBytes)
			result, err := p.Authenticate(context.Background(), presentedSessionCredential(t, id))
			if err != nil {
				t.Fatalf("Authenticate() error = %v", err)
			}
			if !result.Authenticated() {
				t.Fatalf("Authenticated() = false, want true (id_bytes=%d)", test.idBytes)
			}

			mu.Lock()
			defer mu.Unlock()
			if gotIdleTTL != normalized.idleTTL {
				t.Fatalf("Touch() idleTTL = %v, want %v", gotIdleTTL, normalized.idleTTL)
			}
			if gotTouchInterval != normalized.touchInterval {
				t.Fatalf("Touch() touchInterval = %v, want %v", gotTouchInterval, normalized.touchInterval)
			}
			if !gotDeadlineOK {
				t.Fatal("store call context carried no deadline")
			}
			if gotRemaining <= 0 || gotRemaining > normalized.operationTimeout {
				t.Fatalf("remaining deadline = %v, want in (0, %v]", gotRemaining, normalized.operationTimeout)
			}
		})
	}
}
