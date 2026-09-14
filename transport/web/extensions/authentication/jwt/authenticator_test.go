package jwt

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	jwtlib "github.com/golang-jwt/jwt/v5"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/transport/web"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// newTestPlugin returns a plugin configured with a valid test secret and
// otherwise-default configuration.
func newTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	return configuredPlugin(t, nil)
}

// newTestContextWithHeader builds a *web.Ctx wrapping a bare request carrying
// header=value on it -- the minimum ExtractCredential needs to read an HTTP
// header. An empty value means the header is not set at all.
func newTestContextWithHeader(t *testing.T, header, value string) *web.Ctx {
	t.Helper()
	gc, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if value != "" {
		req.Header.Set(header, value)
	}
	gc.Request = req
	return web.NewCtx(gc)
}

func TestPluginExtractCredentialClassifiesAuthorizationHeader(t *testing.T) {
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
			plugin := newTestPlugin(t)
			c := newTestContextWithHeader(t, "Authorization", test.header)
			got, err := plugin.ExtractCredential(c)
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

func TestPluginExtractCredentialUsesConfiguredHeaderAndScheme(t *testing.T) {
	p := configuredPlugin(t, func(cfg *Config) {
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

func TestPluginAuthenticateAcceptsValidTokenAndPublishesPrincipal(t *testing.T) {
	p := newTestPlugin(t)
	token, err := p.Sign("alice", Claims{"role": "admin"})
	if err != nil {
		t.Fatal(err)
	}
	credential, ok := authentication.Presented(token).Credential()
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
	if typed.Subject != "alice" || typed.AuthMethod != "jwt" || typed.Attributes["role"] != "admin" {
		t.Fatalf("principal = %#v", typed)
	}
}

func TestPluginAuthenticateRejectsInvalidCredentialType(t *testing.T) {
	p := newTestPlugin(t)
	credential, ok := authentication.Presented(42).Credential()
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
	if challenges := result.Challenges(); len(challenges) != 1 || challenges[0] != authentication.Challenge("Bearer") {
		t.Fatalf("Challenges() = %v, want [Bearer]", challenges)
	}
}

func signTestToken(t *testing.T, method jwtlib.SigningMethod, claims jwtlib.MapClaims) string {
	t.Helper()
	raw, err := jwtlib.NewWithClaims(method, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign test token: %v", err)
	}
	return raw
}

func TestPluginAuthenticateRejectsInvalidTokens(t *testing.T) {
	p := configuredPlugin(t, func(cfg *Config) {
		cfg.Algorithms = []string{"HS256"}
	})

	expired := signTestToken(t, jwtlib.SigningMethodHS256, jwtlib.MapClaims{
		"sub": "alice",
		"exp": jwtlib.NewNumericDate(fixedNow.Add(-time.Minute)),
	})
	missingExp := signTestToken(t, jwtlib.SigningMethodHS256, jwtlib.MapClaims{"sub": "alice"})
	wrongAlgorithm := signTestToken(t, jwtlib.SigningMethodHS512, jwtlib.MapClaims{
		"sub": "alice",
		"exp": jwtlib.NewNumericDate(fixedNow.Add(time.Hour)),
	})
	noneToken := jwtlib.NewWithClaims(jwtlib.SigningMethodNone, jwtlib.MapClaims{
		"exp": jwtlib.NewNumericDate(fixedNow.Add(time.Hour)),
	})
	none, err := noneToken.SignedString(jwtlib.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaToken := jwtlib.NewWithClaims(jwtlib.SigningMethodRS256, jwtlib.MapClaims{
		"sub": "alice",
		"exp": jwtlib.NewNumericDate(fixedNow.Add(time.Hour)),
	})
	rsaSigned, err := rsaToken.SignedString(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	noSubject := signTestToken(t, jwtlib.SigningMethodHS256, jwtlib.MapClaims{
		"exp": jwtlib.NewNumericDate(fixedNow.Add(time.Hour)),
	})

	tests := []struct {
		name  string
		token string
	}{
		{name: "expired", token: expired},
		{name: "missing exp", token: missingExp},
		{name: "wrong algorithm", token: wrongAlgorithm},
		{name: "none algorithm", token: none},
		{name: "asymmetric algorithm confusion", token: rsaSigned},
		{name: "structurally malformed", token: "not-a-token"},
		{name: "missing subject", token: noSubject},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			credential, ok := authentication.Presented(tt.token).Credential()
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
			kind, ok := result.Rejection()
			if !ok || kind != authentication.RejectionInvalidCredential {
				t.Fatalf("Rejection() = %v, ok=%v, want RejectionInvalidCredential", kind, ok)
			}
			challenges := result.Challenges()
			if len(challenges) != 1 || challenges[0] != authentication.Challenge("Bearer") {
				t.Fatalf("Challenges() = %v, want [Bearer]", challenges)
			}
			reason, ok := result.Reason()
			if !ok {
				t.Fatal("Reason() ok = false")
			}
			// Deliberately do not leak signature, expiry, issuer, or parser
			// detail: the reason is a fixed, generic string regardless of why
			// verification failed, including the missing-subject case.
			const wantReason = "invalid token"
			if string(reason) != wantReason {
				t.Fatalf("reason = %q, want %q", reason, wantReason)
			}
			// The equality check above subsumes this, but it documents the
			// intent explicitly for the next reader.
			if strings.Contains(string(reason), tt.token) {
				t.Fatalf("reason leaked token material: %q", reason)
			}
		})
	}
}
