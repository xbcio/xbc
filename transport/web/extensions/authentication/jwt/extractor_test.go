package jwt

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication"
	jwtcore "github.com/xbcio/xbc/extensions/authentication/jwt"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

const testSecret = "0123456789abcdef0123456789abcdef"

// configuredExtractor builds the neutral authenticator from a valid test
// secret, applies mutate to its configuration, and returns the extractor that
// pairs with it.
func configuredExtractor(t *testing.T, mutate func(*jwtcore.Config)) *Extractor {
	t.Helper()
	cfg := jwtcore.DefaultConfig()
	cfg.Secret = testSecret
	if mutate != nil {
		mutate(&cfg)
	}
	authenticator, err := jwtcore.New(cfg)
	if err != nil {
		t.Fatalf("jwtcore.New() error = %v", err)
	}
	extractor, err := New(authenticator)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return extractor
}

// newTestContextWithHeader builds a *web.Ctx wrapping a bare request carrying
// header=value on it -- the minimum ExtractCredential needs to read an HTTP
// header. An empty value means the header is not set at all.
func newTestContextWithHeader(t *testing.T, header, value string) *web.Ctx {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if value != "" {
		req.Header.Set(header, value)
	}
	return enginetest.NewCtx(httptest.NewRecorder(), req)
}

func TestExtractorExtractCredentialClassifiesAuthorizationHeader(t *testing.T) {
	t.Parallel()

	const wantChallenge = authentication.Challenge("Bearer")

	tests := []struct {
		name          string
		header        string
		want          authentication.CredentialStatus
		wantChallenge bool
	}{
		{name: "no header is absent", header: "", want: authentication.CredentialStatusAbsent, wantChallenge: true},
		{name: "other scheme is absent", header: "ApiKey abc", want: authentication.CredentialStatusAbsent, wantChallenge: true},
		{name: "bearer with no token is malformed", header: "Bearer", want: authentication.CredentialStatusMalformed, wantChallenge: true},
		{name: "bearer with empty token is malformed", header: "Bearer  ", want: authentication.CredentialStatusMalformed, wantChallenge: true},
		{name: "bearer with token is presented", header: "Bearer abc.def.ghi", want: authentication.CredentialStatusPresented, wantChallenge: false},
		{name: "structurally wrong jwt is still presented", header: "Bearer not-a-jwt", want: authentication.CredentialStatusPresented, wantChallenge: false},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			extractor := configuredExtractor(t, nil)
			c := newTestContextWithHeader(t, "Authorization", test.header)
			got, err := extractor.ExtractCredential(c)
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
		})
	}
}

func TestExtractorExtractCredentialUsesConfiguredHeaderAndScheme(t *testing.T) {
	p := configuredExtractor(t, func(cfg *jwtcore.Config) {
		cfg.Header = "X-Access-Token"
		cfg.Scheme = "JWT"
	})

	c := newTestContextWithHeader(t, "X-Access-Token", "JWT abc.def.ghi")
	got, err := p.ExtractCredential(c)
	if err != nil {
		t.Fatalf("ExtractCredential() error = %v", err)
	}
	if got.Status() != authentication.CredentialStatusPresented {
		t.Fatalf("status = %v, want Presented", got.Status())
	}
	credential, ok := got.Credential()
	if !ok {
		t.Fatal("Credential() ok = false")
	}
	if token, _ := credential.Value().(string); token != "abc.def.ghi" {
		t.Fatalf("token = %q, want abc.def.ghi", token)
	}

	// The default Authorization header is not this plugin's configured header,
	// so a well-formed bearer token there must not be picked up.
	other := newTestContextWithHeader(t, "Authorization", "JWT abc.def.ghi")
	got, err = p.ExtractCredential(other)
	if err != nil {
		t.Fatalf("ExtractCredential() error = %v", err)
	}
	if got.Status() != authentication.CredentialStatusAbsent {
		t.Fatalf("status = %v, want Absent", got.Status())
	}
	// The challenge must follow the configured scheme, not a hardcoded Bearer.
	if challenge, ok := got.Challenge(); !ok || challenge != authentication.Challenge("JWT") {
		t.Fatalf("Challenge() = %q, ok=%v, want %q", challenge, ok, "JWT")
	}
}

func TestNewRequiresAnAuthenticator(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("New(nil) error = nil, want a missing-authenticator failure")
	}
}

func TestExtractorPairsWithTheNeutralScheme(t *testing.T) {
	if got := configuredExtractor(t, nil).Scheme(); got != jwtcore.Scheme {
		t.Fatalf("Scheme() = %q, want %q", got, jwtcore.Scheme)
	}
}
