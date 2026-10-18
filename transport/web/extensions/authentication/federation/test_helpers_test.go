package federation

import (
	"testing"
	"time"
)

const (
	testProvider = "corp"
	testState    = "0123456789abcdef0123456789abcdef"
	testSecret   = "browser-binding-secret"
	testNow      = "2026-10-18T08:00:00Z"
)

func testTime(t *testing.T) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, testNow)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// testTransaction builds a valid transaction for the store and login tests.
func testTransaction(now time.Time, ttl time.Duration) Transaction {
	return Transaction{
		Provider:     testProvider,
		State:        testState,
		Nonce:        "nonce-value",
		CodeVerifier: "pkce-verifier-value",
		RequestID:    "saml-request-id",
		ReturnTo:     "/dashboard",
		Binding:      BrowserBinding(testSecret),
		ExpiresAt:    now.Add(ttl),
	}
}

func fixedClock(now *time.Time) func() time.Time {
	return func() time.Time { return *now }
}
