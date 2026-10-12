package jwt

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/xbcio/xbc/extensions/authentication"
	jwtcore "github.com/xbcio/xbc/extensions/authentication/jwt"
)

const testSecret = "0123456789abcdef0123456789abcdef"

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

func TestExtractCredentialClassifiesAuthorizationMetadata(t *testing.T) {
	t.Parallel()
	const wantChallenge = authentication.Challenge("Bearer")
	tests := []struct {
		name      string
		values    []string
		want      authentication.CredentialStatus
		wantToken string
	}{
		{name: "no entry is absent", want: authentication.CredentialStatusAbsent},
		{name: "blank entry is absent", values: []string{"  "}, want: authentication.CredentialStatusAbsent},
		{name: "other scheme is absent", values: []string{"ApiKey abc"}, want: authentication.CredentialStatusAbsent},
		{name: "bearer with no token is malformed", values: []string{"Bearer"}, want: authentication.CredentialStatusMalformed},
		{name: "bearer with extra fields is malformed", values: []string{"Bearer a b"}, want: authentication.CredentialStatusMalformed},
		{name: "two bearer values are malformed", values: []string{"Bearer a.b.c", "Bearer d.e.f"}, want: authentication.CredentialStatusMalformed},
		{name: "bearer with token is presented", values: []string{"Bearer abc.def.ghi"}, want: authentication.CredentialStatusPresented, wantToken: "abc.def.ghi"},
		{name: "scheme match is case-insensitive", values: []string{"bearer abc.def.ghi"}, want: authentication.CredentialStatusPresented, wantToken: "abc.def.ghi"},
		{name: "structurally wrong jwt is still presented", values: []string{"Bearer not-a-jwt"}, want: authentication.CredentialStatusPresented, wantToken: "not-a-jwt"},
		// Another scheme's value beside a bearer one belongs to that scheme.
		{name: "bearer beside another scheme is presented", values: []string{"ApiKey k", "Bearer abc.def.ghi"}, want: authentication.CredentialStatusPresented, wantToken: "abc.def.ghi"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			md := metadata.MD{}
			for _, value := range test.values {
				md.Append("authorization", value)
			}
			got, err := configuredExtractor(t, nil).ExtractCredential(context.Background(), md)
			if err != nil {
				t.Fatalf("ExtractCredential() error = %v", err)
			}
			if got.Status() != test.want {
				t.Fatalf("status = %v, want %v", got.Status(), test.want)
			}
			challenge, hasChallenge := got.Challenge()
			if test.want == authentication.CredentialStatusPresented {
				credential, _ := got.Credential()
				if token, ok := authentication.CredentialAs[string](credential); !ok || token != test.wantToken {
					t.Fatalf("token = %q, want %q", token, test.wantToken)
				}
				if hasChallenge {
					t.Fatalf("Challenge() = %q, want none", challenge)
				}
				return
			}
			if !hasChallenge || challenge != wantChallenge {
				t.Fatalf("Challenge() = %q, ok=%v, want %q", challenge, hasChallenge, wantChallenge)
			}
		})
	}
}

func TestExtractCredentialReadsTheConfiguredHeaderLowerCased(t *testing.T) {
	t.Parallel()
	e := configuredExtractor(t, func(cfg *jwtcore.Config) {
		cfg.Header = "X-Auth-Token"
		cfg.Scheme = "Token"
	})
	got, _ := e.ExtractCredential(context.Background(), metadata.Pairs("X-Auth-Token", "Token abc.def.ghi"))
	credential, _ := got.Credential()
	if token, ok := authentication.CredentialAs[string](credential); !ok || token != "abc.def.ghi" {
		t.Fatalf("token = %q, %v; want the configured entry and scheme", token, ok)
	}
	if got, _ := e.ExtractCredential(context.Background(), metadata.Pairs("authorization", "Bearer abc.def.ghi")); got.Status() != authentication.CredentialStatusAbsent {
		t.Fatalf("status = %v, want Absent for the unconfigured default entry", got.Status())
	}
}
