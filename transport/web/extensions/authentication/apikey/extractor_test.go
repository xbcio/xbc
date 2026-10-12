package apikey

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication"
	apikeycore "github.com/xbcio/xbc/extensions/authentication/apikey"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

// configuredExtractor returns an extractor paired with a neutral
// authenticator built from the default configuration plus mutate. The one
// static credential only satisfies the authenticator's repository
// requirement; extraction never consults it.
func configuredExtractor(t *testing.T, mutate func(*apikeycore.Config)) *Extractor {
	t.Helper()
	cfg := apikeycore.DefaultConfig()
	cfg.Static = []apikeycore.StaticCredential{{
		ID:      "payments",
		AppID:   "checkout",
		Subject: "service:payments",
		SHA256:  apikeycore.HashKey("0123456789abcdef0123456789abcdef").String(),
	}}
	if mutate != nil {
		mutate(&cfg)
	}
	authenticator, err := apikeycore.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(authenticator)
	if err != nil {
		t.Fatal(err)
	}
	return e
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

func TestExtractorExtractCredentialClassifiesHeaders(t *testing.T) {
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
			p := configuredExtractor(t, nil)
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

// TestExtractorExtractCredentialRespectsAllowBearer pins Config.AllowBearer as a
// live control, not dead configuration: with it disabled, Authorization is
// not one of apikey's credential sources at all, so both a well-formed and a
// duplicated Authorization value must be Absent rather than Presented or
// Malformed. Without this test, deleting the AllowBearer check entirely
// leaves the whole package green.
func TestExtractorExtractCredentialRespectsAllowBearer(t *testing.T) {
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
			p := configuredExtractor(t, func(cfg *apikeycore.Config) { cfg.AllowBearer = false })
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

// TestExtractorExtractCredentialUsesConfiguredHeaderNamesAndScheme pins
// Config.Header, Config.AppIDHeader, and Config.BearerScheme as live
// configuration: every other test in this package uses DefaultConfig(), so
// hardcoding "X-API-Key", "X-App-ID", or "Bearer" in place of the configured
// fields would leave the rest of the suite green.
func TestExtractorExtractCredentialUsesConfiguredHeaderNamesAndScheme(t *testing.T) {
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
			p := configuredExtractor(t, func(cfg *apikeycore.Config) {
				cfg.Header = "X-Custom-Key"
				cfg.AppIDHeader = "X-Custom-App-ID"
				cfg.BearerScheme = "XBC-Key"
			})
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

func TestExtractorExtractCredentialCarriesSecretAndAppID(t *testing.T) {
	p := configuredExtractor(t, nil)

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
	value, ok := credential.Value().(apikeycore.PresentedKey)
	if !ok {
		t.Fatalf("credential value type = %T, want apikeycore.PresentedKey", credential.Value())
	}
	if value.Secret != "0123456789abcdef0123456789abcdef" || value.AppID != "checkout" {
		t.Fatalf("PresentedKey = %#v", value)
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
	value, ok = credential.Value().(apikeycore.PresentedKey)
	if !ok {
		t.Fatalf("credential value type = %T, want apikeycore.PresentedKey", credential.Value())
	}
	if value.Secret != "0123456789abcdef0123456789abcdef" || value.AppID != "checkout" {
		t.Fatalf("PresentedKey = %#v", value)
	}
}

func TestNewRequiresAnAuthenticator(t *testing.T) {
	if e, err := New(nil); err == nil || e != nil {
		t.Fatalf("New(nil) = %#v, %v; want nil extractor and error", e, err)
	}
}

func TestExtractorPairsWithTheNeutralScheme(t *testing.T) {
	if got := configuredExtractor(t, nil).Scheme(); got != apikeycore.Scheme {
		t.Fatalf("Scheme() = %q, want %q", got, apikeycore.Scheme)
	}
}
