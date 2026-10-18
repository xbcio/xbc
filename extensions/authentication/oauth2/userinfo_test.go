package oauth2

import (
	"context"
	"strings"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication/federation/outbound"
)

func TestUserInfoRequiresTheConfiguredSubjectField(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	// The document carries an email address but no stable account identifier.
	// An email is mutable and reassignable, so it must never become the subject.
	idp.setUserInfo(func(idp *fakeIDP) {
		idp.userInfoJSON = map[string]any{
			"email":   "alice@example.com",
			"profile": map[string]any{"name": "Alice"},
		}
	})

	_, err := idp.exchange(t, provider)
	requireErrorIs(t, err, ErrInvalidUserInfo, "Exchange")
	if !strings.Contains(err.Error(), `"sub"`) {
		t.Fatalf("error = %q, want the missing subject field named", err)
	}
}

func TestUserInfoSubjectIsTheStableIdentifierNotTheEmail(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)

	identity, err := idp.exchange(t, provider)
	requireNoError(t, err, "Exchange")
	if identity.Subject != testSubject {
		t.Fatalf("Subject = %q, want the stable identifier %q", identity.Subject, testSubject)
	}
	if identity.Subject == identity.Claims["email"] {
		t.Fatalf("Subject = %q, the email address was used as the subject", identity.Subject)
	}
}

func TestUserInfoMapsANestedSubjectField(t *testing.T) {
	idp := newFakeIDP(t)
	config := idp.providerConfig()
	config.SubjectField = "account.id"
	config.ClaimFields = nil
	provider, err := New(config)
	requireNoError(t, err, "New")
	idp.setUserInfo(func(idp *fakeIDP) {
		idp.userInfoJSON = map[string]any{
			"email":   "alice@example.com",
			"account": map[string]any{"id": "nested-account-7"},
		}
	})

	identity, err := idp.exchange(t, provider)
	requireNoError(t, err, "Exchange")
	if identity.Subject != "nested-account-7" {
		t.Fatalf("Subject = %q, want the nested account identifier", identity.Subject)
	}
}

func TestUserInfoRejectsAnUnusableSubjectValue(t *testing.T) {
	for name, document := range map[string]map[string]any{
		"number":     {"sub": 42},
		"empty":      {"sub": ""},
		"whitespace": {"sub": "   "},
		"array":      {"sub": []any{"id"}},
		"object":     {"sub": map[string]any{"id": "nested"}},
		"null":       {"sub": nil},
	} {
		t.Run(name, func(t *testing.T) {
			idp := newFakeIDP(t)
			config := idp.providerConfig()
			// No claims are mapped, so the only possible failure is the subject.
			config.ClaimFields = nil
			provider, err := New(config)
			requireNoError(t, err, "New")
			idp.setUserInfo(func(idp *fakeIDP) { idp.userInfoJSON = document })

			_, err = idp.exchange(t, provider)
			requireErrorIs(t, err, ErrInvalidUserInfo, "Exchange")
		})
	}
}

func TestUserInfoRejectsAMissingMappedClaim(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	// profile.name is configured as the "name" claim; a response without it
	// must fail the login instead of guessing or defaulting the value.
	idp.setUserInfo(func(idp *fakeIDP) {
		idp.userInfoJSON = map[string]any{
			"sub":   testSubject,
			"email": "alice@example.com",
		}
	})

	_, err := idp.exchange(t, provider)
	requireErrorIs(t, err, ErrInvalidUserInfo, "Exchange")
	if !strings.Contains(err.Error(), `"profile.name"`) {
		t.Fatalf("error = %q, want the missing claim path named", err)
	}
}

func TestUserInfoRejectsANonObjectBody(t *testing.T) {
	for name, raw := range map[string]string{
		"array":     `[{"sub":"id"}]`,
		"null":      `null`,
		"string":    `"sub"`,
		"malformed": `{"sub":`,
		"empty":     ` `,
	} {
		t.Run(name, func(t *testing.T) {
			idp := newFakeIDP(t)
			provider := idp.newProvider(t)
			idp.setUserInfo(func(idp *fakeIDP) { idp.userInfoRawBody = raw })

			_, err := idp.exchange(t, provider)
			requireErrorIs(t, err, ErrInvalidUserInfo, "Exchange")
		})
	}
}

func TestUserInfoRejectsAnOversizedBody(t *testing.T) {
	idp := newFakeIDP(t)
	config := idp.providerConfig()
	config.Policy.MaxBodyBytes = 32
	provider, err := New(config)
	requireNoError(t, err, "New")

	_, err = provider.fetchUserInfo(context.Background(), testAccessToken)
	requireErrorIs(t, err, ErrUserInfo, "fetchUserInfo")
	if !strings.Contains(err.Error(), outbound.ErrBodyTooLarge.Error()) {
		t.Fatalf("error = %v, want the body limit rejection", err)
	}
}

func TestUserInfoRejectsAPrivateAddress(t *testing.T) {
	for name, endpoint := range map[string]string{
		"loopback":        "https://127.0.0.1/userinfo",
		"cloud metadata":  "https://169.254.169.254/latest/meta-data",
		"private network": "https://10.1.2.3/userinfo",
		"unique local v6": "https://[fc00::1]/userinfo",
		"link-local v6":   "https://[fe80::1]/userinfo",
	} {
		t.Run(name, func(t *testing.T) {
			idp := newFakeIDP(t)
			config := idp.providerConfig()
			config.UserInfoEndpoint = endpoint
			config.Policy.AllowPrivate = false
			provider, err := New(config)
			requireNoError(t, err, "New")

			_, err = provider.fetchUserInfo(context.Background(), testAccessToken)
			requireErrorIs(t, err, ErrUserInfo, "fetchUserInfo")
			if !strings.Contains(err.Error(), outbound.ErrAddressRejected.Error()) {
				t.Fatalf("error = %v, want the address policy rejection", err)
			}
		})
	}
}

func TestUserInfoNeverEchoesTheProviderBodyOrCredentials(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	idp.setUserInfo(func(idp *fakeIDP) {
		idp.userInfoStatus = 500
		idp.userInfoRawBody = `{"error":"raw provider text"}`
	})

	_, err := idp.exchange(t, provider)
	requireErrorIs(t, err, ErrUserInfo, "Exchange")
	text := err.Error()
	if !strings.Contains(text, "status 500") {
		t.Fatalf("error = %q, want the status code", text)
	}
	for name, secret := range map[string]string{
		"provider body": `raw provider text`,
		"access token":  testAccessToken,
		"client secret": testClientSecret,
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("error carries the %s: %q", name, text)
		}
	}
}

func TestFieldAtResolvesDottedPaths(t *testing.T) {
	document := map[string]any{
		"sub":     "id-1",
		"profile": map[string]any{"name": "Alice", "empty": map[string]any{}},
		"scalar":  "value",
	}
	for name, testCase := range map[string]struct {
		path []string
		want any
		ok   bool
	}{
		"top level":               {path: []string{"sub"}, want: "id-1", ok: true},
		"nested":                  {path: []string{"profile", "name"}, want: "Alice", ok: true},
		"empty object":            {path: []string{"profile", "empty"}, want: map[string]any{}, ok: true},
		"missing key":             {path: []string{"missing"}, ok: false},
		"missing nested key":      {path: []string{"profile", "missing"}, ok: false},
		"non-object intermediate": {path: []string{"scalar", "name"}, ok: false},
		"path into empty object":  {path: []string{"profile", "empty", "name"}, ok: false},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := fieldAt(document, testCase.path)
			if ok != testCase.ok {
				t.Fatalf("fieldAt(%v) ok = %v, want %v", testCase.path, ok, testCase.ok)
			}
			if testCase.ok && !equalValue(got, testCase.want) {
				t.Fatalf("fieldAt(%v) = %v, want %v", testCase.path, got, testCase.want)
			}
		})
	}
}

func equalValue(left, right any) bool {
	leftMap, leftOK := left.(map[string]any)
	rightMap, rightOK := right.(map[string]any)
	if leftOK || rightOK {
		if !leftOK || !rightOK || len(leftMap) != len(rightMap) {
			return false
		}
		for key, value := range leftMap {
			other, exists := rightMap[key]
			if !exists || !equalValue(value, other) {
				return false
			}
		}
		return true
	}
	return left == right
}
