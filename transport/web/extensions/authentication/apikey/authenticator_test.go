package apikey

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

// configuredPlugin returns a plugin configured with one static credential
// whose key is "0123456789abcdef0123456789abcdef", app ID "checkout", and
// subject "service:payments".
func configuredPlugin(t *testing.T, requireAppID bool) *Plugin {
	t.Helper()
	cfg := DefaultConfig()
	cfg.RequireAppID = requireAppID
	cfg.Static = []StaticCredential{{
		ID:      "payments",
		AppID:   "checkout",
		Subject: "service:payments",
		SHA256:  HashKey("0123456789abcdef0123456789abcdef").String(),
		Attributes: map[string]any{
			"role": "writer",
		},
	}}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// newTestContextWithHeaders builds a bare *web.Ctx carrying the given headers
// on its request. A nil slice for a header name means the header is not set;
// multiple values mean the header line is duplicated.
func newTestContextWithHeaders(t *testing.T, headers map[string][]string) *web.Ctx {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	return enginetest.NewCtx(httptest.NewRecorder(), req)
}

func TestPluginExtractCredentialClassifiesHeaders(t *testing.T) {
	t.Parallel()

	const wantChallenge = authentication.Challenge("Bearer")

	tests := []struct {
		name          string
		headers       map[string][]string
		want          authentication.CredentialStatus
		wantChallenge bool
		wantReason    authentication.SafeReason
	}{
		{
			name:          "no headers is absent",
			headers:       nil,
			want:          authentication.CredentialStatusAbsent,
			wantChallenge: true,
		},
		{
			name:          "only key header single non-empty value is presented",
			headers:       map[string][]string{"X-API-Key": {"0123456789abcdef0123456789abcdef"}},
			want:          authentication.CredentialStatusPresented,
			wantChallenge: false,
		},
		{
			name: "only Authorization with AllowBearer, matching prefix and non-empty token is presented",
			headers: map[string][]string{
				"Authorization": {"Bearer 0123456789abcdef0123456789abcdef"},
			},
			want:          authentication.CredentialStatusPresented,
			wantChallenge: false,
		},
		{
			name:          "only Authorization but prefix is not this scheme is absent",
			headers:       map[string][]string{"Authorization": {"ApiKey 0123456789abcdef0123456789abcdef"}},
			want:          authentication.CredentialStatusAbsent,
			wantChallenge: true,
		},
		{
			name:          "key header duplicated is malformed",
			headers:       map[string][]string{"X-API-Key": {"aaa", "bbb"}},
			want:          authentication.CredentialStatusMalformed,
			wantChallenge: true,
			wantReason:    "duplicate api key header",
		},
		{
			name:          "key header present but blank is malformed",
			headers:       map[string][]string{"X-API-Key": {"   "}},
			want:          authentication.CredentialStatusMalformed,
			wantChallenge: true,
			wantReason:    "empty api key header",
		},
		{
			name: "key header and Authorization header both present is malformed",
			headers: map[string][]string{
				"X-API-Key":     {"0123456789abcdef0123456789abcdef"},
				"Authorization": {"Bearer 0123456789abcdef0123456789abcdef"},
			},
			want:          authentication.CredentialStatusMalformed,
			wantChallenge: true,
			wantReason:    "conflicting api key headers",
		},
		{
			name: "app ID header duplicated alongside a valid key header is malformed",
			headers: map[string][]string{
				"X-API-Key": {"0123456789abcdef0123456789abcdef"},
				"X-App-ID":  {"checkout", "other"},
			},
			want:          authentication.CredentialStatusMalformed,
			wantChallenge: true,
			wantReason:    "malformed app id header",
		},
		{
			// This request names nothing of apikey's: no key header, no
			// Authorization value. A duplicated X-App-ID alone must not turn
			// into a Malformed reason, or an unauthenticated caller gets a
			// free, credential-less probe for whether apikey is in this
			// route's scheme selection.
			name:          "app ID header duplicated alone with no api key or Authorization is absent",
			headers:       map[string][]string{"X-App-ID": {"checkout", "other"}},
			want:          authentication.CredentialStatusAbsent,
			wantChallenge: true,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p := configuredPlugin(t, false)
			c := newTestContextWithHeaders(t, test.headers)
			got, err := p.ExtractCredential(c)
			if err != nil {
				t.Fatalf("ExtractCredential() error = %v", err)
			}
			if got.Status() != test.want {
				t.Fatalf("status = %v, want %v", got.Status(), test.want)
			}
			challenge, ok := got.Challenge()
			if test.wantChallenge {
				if !ok || challenge != wantChallenge {
					t.Fatalf("Challenge() = %q, ok=%v, want %q", challenge, ok, wantChallenge)
				}
			} else if ok {
				t.Fatalf("Challenge() = %q, want none", challenge)
			}
			if test.want == authentication.CredentialStatusMalformed {
				reason, ok := got.Reason()
				if !ok || reason != test.wantReason {
					t.Fatalf("Reason() = %q, ok=%v, want %q", reason, ok, test.wantReason)
				}
			}
		})
	}
}

// configuredPluginWithoutBearer returns a plugin identical to configuredPlugin
// except AllowBearer is disabled, so the Authorization header is not one of
// apikey's credential sources.
func configuredPluginWithoutBearer(t *testing.T) *Plugin {
	t.Helper()
	cfg := DefaultConfig()
	cfg.AllowBearer = false
	cfg.Static = []StaticCredential{{
		ID:      "payments",
		AppID:   "checkout",
		Subject: "service:payments",
		SHA256:  HashKey("0123456789abcdef0123456789abcdef").String(),
	}}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestPluginExtractCredentialRespectsAllowBearer pins Config.AllowBearer as a
// live control, not dead configuration: with it disabled, Authorization is
// not one of apikey's credential sources at all, so both a well-formed and a
// duplicated Authorization value must be Absent rather than Presented or
// Malformed. Without this test, deleting the AllowBearer check entirely
// leaves the whole package green.
func TestPluginExtractCredentialRespectsAllowBearer(t *testing.T) {
	t.Parallel()

	const wantChallenge = authentication.Challenge("Bearer")

	tests := []struct {
		name    string
		headers map[string][]string
	}{
		{
			name:    "matching bearer prefix is absent when allow bearer is disabled",
			headers: map[string][]string{"Authorization": {"Bearer 0123456789abcdef0123456789abcdef"}},
		},
		{
			name:    "duplicated Authorization is absent when allow bearer is disabled",
			headers: map[string][]string{"Authorization": {"Bearer aaa", "Bearer bbb"}},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p := configuredPluginWithoutBearer(t)
			c := newTestContextWithHeaders(t, test.headers)
			got, err := p.ExtractCredential(c)
			if err != nil {
				t.Fatalf("ExtractCredential() error = %v", err)
			}
			if got.Status() != authentication.CredentialStatusAbsent {
				t.Fatalf("status = %v, want %v", got.Status(), authentication.CredentialStatusAbsent)
			}
			challenge, ok := got.Challenge()
			if !ok || challenge != wantChallenge {
				t.Fatalf("Challenge() = %q, ok=%v, want %q", challenge, ok, wantChallenge)
			}
		})
	}
}

// configuredPluginWithHeaders returns a plugin configured with a non-default
// credential header, app-ID header, and bearer scheme, so tests can prove
// those three configured fields are actually consulted rather than the
// package's compiled-in defaults ("X-API-Key", "X-App-ID", "Bearer").
func configuredPluginWithHeaders(t *testing.T) *Plugin {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Header = "X-Custom-Key"
	cfg.AppIDHeader = "X-Custom-App-ID"
	cfg.BearerScheme = "XBC-Key"
	cfg.Static = []StaticCredential{{
		ID:      "payments",
		AppID:   "checkout",
		Subject: "service:payments",
		SHA256:  HashKey("0123456789abcdef0123456789abcdef").String(),
	}}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestPluginExtractCredentialUsesConfiguredHeaderNamesAndScheme pins
// Config.Header, Config.AppIDHeader, and Config.BearerScheme as live
// configuration: every other test in this package uses DefaultConfig(), so
// hardcoding "X-API-Key", "X-App-ID", or "Bearer" in place of the configured
// fields would leave the rest of the suite green.
func TestPluginExtractCredentialUsesConfiguredHeaderNamesAndScheme(t *testing.T) {
	t.Parallel()

	const wantChallenge = authentication.Challenge("XBC-Key")

	tests := []struct {
		name          string
		headers       map[string][]string
		want          authentication.CredentialStatus
		wantChallenge bool
		wantReason    authentication.SafeReason
	}{
		{
			name:          "configured key header is presented",
			headers:       map[string][]string{"X-Custom-Key": {"0123456789abcdef0123456789abcdef"}},
			want:          authentication.CredentialStatusPresented,
			wantChallenge: false,
		},
		{
			name: "configured bearer scheme is presented",
			headers: map[string][]string{
				"Authorization": {"XBC-Key 0123456789abcdef0123456789abcdef"},
			},
			want:          authentication.CredentialStatusPresented,
			wantChallenge: false,
		},
		{
			name:          "the compiled-in default Bearer prefix no longer matches the configured scheme",
			headers:       map[string][]string{"Authorization": {"Bearer 0123456789abcdef0123456789abcdef"}},
			want:          authentication.CredentialStatusAbsent,
			wantChallenge: true,
		},
		{
			name: "configured app id header duplicated is malformed",
			headers: map[string][]string{
				"X-Custom-Key":    {"0123456789abcdef0123456789abcdef"},
				"X-Custom-App-ID": {"a", "b"},
			},
			want:          authentication.CredentialStatusMalformed,
			wantChallenge: true,
			wantReason:    "malformed app id header",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p := configuredPluginWithHeaders(t)
			c := newTestContextWithHeaders(t, test.headers)
			got, err := p.ExtractCredential(c)
			if err != nil {
				t.Fatalf("ExtractCredential() error = %v", err)
			}
			if got.Status() != test.want {
				t.Fatalf("status = %v, want %v", got.Status(), test.want)
			}
			challenge, ok := got.Challenge()
			if test.wantChallenge {
				if !ok || challenge != wantChallenge {
					t.Fatalf("Challenge() = %q, ok=%v, want %q", challenge, ok, wantChallenge)
				}
			} else if ok {
				t.Fatalf("Challenge() = %q, want none", challenge)
			}
			if test.want == authentication.CredentialStatusMalformed {
				reason, ok := got.Reason()
				if !ok || reason != test.wantReason {
					t.Fatalf("Reason() = %q, ok=%v, want %q", reason, ok, test.wantReason)
				}
			}
		})
	}
}

// TestPluginAuthenticateUsesConfiguredBearerSchemeForChallenge pins
// Authenticate's own Challenge derivation to the configured BearerScheme,
// independent of the extractor-side coverage above.
func TestPluginAuthenticateUsesConfiguredBearerSchemeForChallenge(t *testing.T) {
	p := configuredPluginWithHeaders(t)
	credential, ok := authentication.Presented("not-a-credentialValue").Credential()
	if !ok {
		t.Fatal("Credential() ok = false")
	}

	result, err := p.Authenticate(context.Background(), credential)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if !result.Rejected() {
		t.Fatal("Rejected() = false, want true")
	}
	if challenges := result.Challenges(); len(challenges) != 1 || challenges[0] != authentication.Challenge("XBC-Key") {
		t.Fatalf("Challenges() = %v, want [XBC-Key]", challenges)
	}
}

func TestPluginExtractCredentialCarriesSecretAndAppID(t *testing.T) {
	p := configuredPlugin(t, false)

	fromKeyHeader := newTestContextWithHeaders(t, map[string][]string{
		"X-API-Key": {"0123456789abcdef0123456789abcdef"},
		"X-App-ID":  {"checkout"},
	})
	got, err := p.ExtractCredential(fromKeyHeader)
	if err != nil {
		t.Fatalf("ExtractCredential() error = %v", err)
	}
	credential, ok := got.Credential()
	if !ok {
		t.Fatal("Credential() ok = false")
	}
	value, ok := credential.Value().(credentialValue)
	if !ok {
		t.Fatalf("credential value type = %T, want credentialValue", credential.Value())
	}
	if value.secret != "0123456789abcdef0123456789abcdef" || value.appID != "checkout" {
		t.Fatalf("credentialValue = %#v", value)
	}

	fromBearer := newTestContextWithHeaders(t, map[string][]string{
		"Authorization": {"Bearer 0123456789abcdef0123456789abcdef"},
		"X-App-ID":      {"checkout"},
	})
	got, err = p.ExtractCredential(fromBearer)
	if err != nil {
		t.Fatalf("ExtractCredential() error = %v", err)
	}
	credential, ok = got.Credential()
	if !ok {
		t.Fatal("Credential() ok = false")
	}
	value, ok = credential.Value().(credentialValue)
	if !ok {
		t.Fatalf("credential value type = %T, want credentialValue", credential.Value())
	}
	if value.secret != "0123456789abcdef0123456789abcdef" || value.appID != "checkout" {
		t.Fatalf("credentialValue = %#v", value)
	}
}

func presentedCredential(t *testing.T, value credentialValue) authentication.Credential {
	t.Helper()
	credential, ok := authentication.Presented(value).Credential()
	if !ok {
		t.Fatal("Credential() ok = false")
	}
	return credential
}

func TestPluginAuthenticateAcceptsValidCredentialAndPublishesPrincipal(t *testing.T) {
	p := configuredPlugin(t, true)
	credential := presentedCredential(t, credentialValue{
		secret: "0123456789abcdef0123456789abcdef",
		appID:  "checkout",
	})

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
	if typed.Subject != "service:payments" || typed.AuthMethod != "apikey" ||
		typed.Attributes["role"] != "writer" || typed.Attributes["credential_id"] != "payments" ||
		typed.Attributes["app_id"] != "checkout" {
		t.Fatalf("principal = %#v", typed)
	}
}

func TestPluginAuthenticateRejectsInvalidCredentialType(t *testing.T) {
	p := configuredPlugin(t, false)
	credential, ok := authentication.Presented("not-a-credentialValue").Credential()
	if !ok {
		t.Fatal("Credential() ok = false")
	}

	result, err := p.Authenticate(context.Background(), credential)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if !result.Rejected() {
		t.Fatal("Rejected() = false, want true")
	}
	reason, ok := result.Reason()
	if !ok || reason != "invalid credential type" {
		t.Fatalf("Reason() = %q, ok=%v, want %q", reason, ok, "invalid credential type")
	}
	if challenges := result.Challenges(); len(challenges) != 1 || challenges[0] != authentication.Challenge("Bearer") {
		t.Fatalf("Challenges() = %v, want [Bearer]", challenges)
	}
}

func TestPluginAuthenticateRejectsShortKey(t *testing.T) {
	p := configuredPlugin(t, false)
	credential := presentedCredential(t, credentialValue{secret: "short"})

	result, err := p.Authenticate(context.Background(), credential)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if !result.Rejected() {
		t.Fatal("Rejected() = false, want true")
	}
	reason, ok := result.Reason()
	if !ok || reason != "invalid credential" {
		t.Fatalf("Reason() = %q, ok=%v, want %q", reason, ok, "invalid credential")
	}
}

func TestPluginAuthenticateRequiresAppIDWhenConfigured(t *testing.T) {
	p := configuredPlugin(t, true)
	credential := presentedCredential(t, credentialValue{secret: "0123456789abcdef0123456789abcdef"})

	result, err := p.Authenticate(context.Background(), credential)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if !result.Rejected() {
		t.Fatal("Rejected() = false, want true")
	}
	reason, ok := result.Reason()
	if !ok || reason != "invalid credential" {
		t.Fatalf("Reason() = %q, ok=%v, want %q", reason, ok, "invalid credential")
	}
}

// erroringRepository always fails Lookup, simulating a storage failure.
type erroringRepository struct{}

func (erroringRepository) Lookup(context.Context, string, KeyDigest) (Credential, bool, error) {
	return Credential{}, false, errors.New("boom")
}

// blankSubjectRepository finds a credential with no subject, which must be
// treated identically to "not found".
type blankSubjectRepository struct{}

func (blankSubjectRepository) Lookup(context.Context, string, KeyDigest) (Credential, bool, error) {
	return Credential{Subject: "   "}, true, nil
}

// TestPluginAuthenticateCollapsesRepositoryFailureReasons pins the existing
// collapse (middleware.go's original comment: repository errors, not-found
// keys, and wrong keys must not be distinguishable) so a future edit cannot
// reintroduce a credential-enumeration oracle through the reason text.
func TestPluginAuthenticateCollapsesRepositoryFailureReasons(t *testing.T) {
	const wantReason = authentication.SafeReason("invalid credential")

	secret := "0123456789abcdef0123456789abcdef"
	wrongSecret := "ffffffffffffffffffffffffffffffff"

	tests := []struct {
		name string
		p    *Plugin
		key  string
	}{
		{name: "wrong key against static repository", p: configuredPlugin(t, false), key: wrongSecret},
		{name: "repository error", p: mustNewWithRepository(t, erroringRepository{}), key: secret},
		{name: "blank subject", p: mustNewWithRepository(t, blankSubjectRepository{}), key: secret},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			credential := presentedCredential(t, credentialValue{secret: tt.key})
			result, err := tt.p.Authenticate(context.Background(), credential)
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
		})
	}
}

func mustNewWithRepository(t *testing.T, repository Repository) *Plugin {
	t.Helper()
	p, err := New(DefaultConfig(), WithRepository(repository))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReplaceStaticCredentialsRotatesAcceptedCredential(t *testing.T) {
	p := configuredPlugin(t, false)
	oldSecret := "0123456789abcdef0123456789abcdef"
	newSecret := "abcdef0123456789abcdef0123456789"

	if result, err := p.Authenticate(context.Background(), presentedCredential(t, credentialValue{secret: oldSecret})); err != nil || !result.Authenticated() {
		t.Fatalf("old key: result=%#v err=%v", result, err)
	}

	if err := p.ReplaceStaticCredentials([]StaticCredential{{
		ID: "payments", AppID: "checkout", Subject: "service:payments", SHA256: HashKey(newSecret).String(),
	}}); err != nil {
		t.Fatal(err)
	}

	if result, err := p.Authenticate(context.Background(), presentedCredential(t, credentialValue{secret: oldSecret})); err != nil || !result.Rejected() {
		t.Fatalf("old key after rotation: result=%#v err=%v", result, err)
	}
	if result, err := p.Authenticate(context.Background(), presentedCredential(t, credentialValue{secret: newSecret})); err != nil || !result.Authenticated() {
		t.Fatalf("new key after rotation: result=%#v err=%v", result, err)
	}
}
