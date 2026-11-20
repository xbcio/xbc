package apikey

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/xbcio/xbc/extensions/authentication"
	apikeycore "github.com/xbcio/xbc/extensions/authentication/apikey"
)

const testKey = "0123456789abcdef0123456789abcdef"

// configuredExtractor returns an extractor paired with a neutral authenticator
// built from the default configuration plus mutate. The static credential only
// satisfies the authenticator's repository requirement.
func configuredExtractor(t *testing.T, mutate func(*apikeycore.Config)) *Extractor {
	t.Helper()
	cfg := apikeycore.DefaultConfig()
	cfg.Static = []apikeycore.StaticCredential{{
		ID:      "payments",
		AppID:   "checkout",
		Subject: "service:payments",
		SHA256:  apikeycore.HashKey(testKey).String(),
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

// incoming builds metadata the way grpc-go hands it to a server: keys
// lower-cased, repeated keys accumulated.
func incoming(pairs ...string) metadata.MD { return metadata.Pairs(pairs...) }

func TestExtractCredentialClassifiesMetadata(t *testing.T) {
	t.Parallel()
	const wantChallenge = authentication.Challenge("Bearer")
	tests := []struct {
		name       string
		md         metadata.MD
		want       authentication.CredentialStatus
		wantSecret string
		wantAppID  string
		wantReason authentication.SafeReason
	}{
		{name: "no metadata is absent", md: nil, want: authentication.CredentialStatusAbsent},
		{name: "key entry is presented", md: incoming("x-api-key", testKey, "x-app-id", "checkout"),
			want: authentication.CredentialStatusPresented, wantSecret: testKey, wantAppID: "checkout"},
		// metadata.Pairs lower-cases keys exactly as the wire does, so a
		// client spelling the configured header in its canonical case is read.
		{name: "canonical-case key is read lower-cased", md: incoming("X-API-Key", testKey),
			want: authentication.CredentialStatusPresented, wantSecret: testKey},
		{name: "bearer authorization is presented", md: incoming("authorization", "Bearer "+testKey),
			want: authentication.CredentialStatusPresented, wantSecret: testKey},
		{name: "another scheme's authorization is absent", md: incoming("authorization", "ApiKey "+testKey),
			want: authentication.CredentialStatusAbsent},
		{name: "bearer without a value is malformed", md: incoming("authorization", "Bearer"),
			want: authentication.CredentialStatusMalformed, wantReason: "malformed credential"},
		{name: "repeated authorization is malformed", md: incoming("authorization", "Bearer a", "authorization", "Bearer b"),
			want: authentication.CredentialStatusMalformed, wantReason: "malformed credential"},
		{name: "repeated key entry is malformed", md: incoming("x-api-key", "aaa", "x-api-key", "bbb"),
			want: authentication.CredentialStatusMalformed, wantReason: "duplicate api key metadata"},
		{name: "blank key entry is malformed", md: incoming("x-api-key", "   "),
			want: authentication.CredentialStatusMalformed, wantReason: "empty api key metadata"},
		{name: "key and authorization together are malformed", md: incoming("x-api-key", testKey, "authorization", "Bearer "+testKey),
			want: authentication.CredentialStatusMalformed, wantReason: "conflicting api key metadata"},
		{name: "repeated app id beside a key is malformed", md: incoming("x-api-key", testKey, "x-app-id", "a", "x-app-id", "b"),
			want: authentication.CredentialStatusMalformed, wantReason: "malformed app id metadata"},
		// Naming nothing of apikey's, a repeated app id alone must stay Absent:
		// a Malformed reason here would let a caller probe the selection.
		{name: "repeated app id alone is absent", md: incoming("x-app-id", "a", "x-app-id", "b"),
			want: authentication.CredentialStatusAbsent},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := configuredExtractor(t, nil).ExtractCredential(context.Background(), test.md)
			if err != nil {
				t.Fatalf("ExtractCredential() error = %v", err)
			}
			if got.Status() != test.want {
				t.Fatalf("status = %v, want %v", got.Status(), test.want)
			}
			switch test.want {
			case authentication.CredentialStatusPresented:
				credential, _ := got.Credential()
				key, ok := authentication.CredentialAs[apikeycore.PresentedKey](credential)
				if !ok || key.Secret != test.wantSecret || key.AppID != test.wantAppID {
					t.Fatalf("credential = %+v, want secret and app id %q", key, test.wantAppID)
				}
			case authentication.CredentialStatusMalformed:
				if reason, ok := got.Reason(); !ok || reason != test.wantReason {
					t.Fatalf("Reason() = %q, want %q", reason, test.wantReason)
				}
				fallthrough
			default:
				if challenge, ok := got.Challenge(); !ok || challenge != wantChallenge {
					t.Fatalf("Challenge() = %q, ok=%v, want %q", challenge, ok, wantChallenge)
				}
			}
		})
	}
}

// TestExtractCredentialRespectsAllowBearer pins AllowBearer as a live control:
// with it off, authorization is not one of apikey's sources at all.
func TestExtractCredentialRespectsAllowBearer(t *testing.T) {
	t.Parallel()
	e := configuredExtractor(t, func(cfg *apikeycore.Config) { cfg.AllowBearer = false })
	for _, md := range []metadata.MD{
		incoming("authorization", "Bearer "+testKey),
		incoming("authorization", "Bearer a", "authorization", "Bearer b"),
	} {
		got, err := e.ExtractCredential(context.Background(), md)
		if err != nil || got.Status() != authentication.CredentialStatusAbsent {
			t.Fatalf("ExtractCredential(%v) = %v, %v; want Absent", md, got.Status(), err)
		}
	}
	// And the key entry still works beside an authorization entry it ignores.
	got, _ := e.ExtractCredential(context.Background(), incoming("x-api-key", testKey, "authorization", "Bearer other"))
	if got.Status() != authentication.CredentialStatusPresented {
		t.Fatalf("status = %v, want Presented", got.Status())
	}
}

func TestExtractCredentialReadsConfiguredNames(t *testing.T) {
	t.Parallel()
	e := configuredExtractor(t, func(cfg *apikeycore.Config) {
		cfg.Header = "X-Service-Key"
		cfg.AppIDHeader = "X-Service-App"
	})
	got, _ := e.ExtractCredential(context.Background(), incoming("x-service-key", testKey, "x-service-app", "checkout"))
	credential, _ := got.Credential()
	key, ok := authentication.CredentialAs[apikeycore.PresentedKey](credential)
	if !ok || key.Secret != testKey || key.AppID != "checkout" {
		t.Fatalf("credential = %+v, %v; want the configured entries", key, ok)
	}
}
