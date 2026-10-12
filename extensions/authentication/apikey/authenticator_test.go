package apikey

import (
	"context"
	"errors"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication"
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

// configuredPluginWithHeaders returns a plugin configured with a non-default
// credential header, app-ID header, and bearer scheme, so tests can prove
// those configured fields are actually consulted rather than the package's
// compiled-in defaults ("X-API-Key", "X-App-ID", "Bearer").
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

// TestPluginAuthenticateUsesConfiguredBearerSchemeForChallenge pins
// Authenticate's own Challenge derivation to the configured BearerScheme,
// independent of any transport adapter's extractor-side coverage.
func TestPluginAuthenticateUsesConfiguredBearerSchemeForChallenge(t *testing.T) {
	p := configuredPluginWithHeaders(t)
	credential, ok := authentication.Presented("not-a-presented-key").Credential()
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

func presentedCredential(t *testing.T, value PresentedKey) authentication.Credential {
	t.Helper()
	credential, ok := authentication.Presented(value).Credential()
	if !ok {
		t.Fatal("Credential() ok = false")
	}
	return credential
}

func TestPluginAuthenticateAcceptsValidCredentialAndPublishesPrincipal(t *testing.T) {
	p := configuredPlugin(t, true)
	credential := presentedCredential(t, PresentedKey{
		Secret: "0123456789abcdef0123456789abcdef",
		AppID:  "checkout",
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
	typed, ok := principal.(authentication.Principal)
	if !ok {
		t.Fatalf("principal type = %T, want authentication.Principal", principal)
	}
	if typed.Subject != "service:payments" || typed.AuthMethod != "apikey" ||
		typed.Attributes["role"] != "writer" || typed.Attributes["credential_id"] != "payments" ||
		typed.Attributes["app_id"] != "checkout" {
		t.Fatalf("principal = %#v", typed)
	}
}

func TestPluginAuthenticateRejectsInvalidCredentialType(t *testing.T) {
	p := configuredPlugin(t, false)
	credential, ok := authentication.Presented("not-a-presented-key").Credential()
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
	credential := presentedCredential(t, PresentedKey{Secret: "short"})

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
	credential := presentedCredential(t, PresentedKey{Secret: "0123456789abcdef0123456789abcdef"})

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
			credential := presentedCredential(t, PresentedKey{Secret: tt.key})
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

	if result, err := p.Authenticate(context.Background(), presentedCredential(t, PresentedKey{Secret: oldSecret})); err != nil || !result.Authenticated() {
		t.Fatalf("old key: result=%#v err=%v", result, err)
	}

	if err := p.ReplaceStaticCredentials([]StaticCredential{{
		ID: "payments", AppID: "checkout", Subject: "service:payments", SHA256: HashKey(newSecret).String(),
	}}); err != nil {
		t.Fatal(err)
	}

	if result, err := p.Authenticate(context.Background(), presentedCredential(t, PresentedKey{Secret: oldSecret})); err != nil || !result.Rejected() {
		t.Fatalf("old key after rotation: result=%#v err=%v", result, err)
	}
	if result, err := p.Authenticate(context.Background(), presentedCredential(t, PresentedKey{Secret: newSecret})); err != nil || !result.Authenticated() {
		t.Fatalf("new key after rotation: result=%#v err=%v", result, err)
	}
}
