package jwt

import (
	"testing"
	"time"

	jwtlib "github.com/golang-jwt/jwt/v5"
)

var fixedNow = time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)

func configuredPlugin(t *testing.T, mutate func(*Config)) *Plugin {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Secret = testSecret
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := newPluginWithClock(cfg, func() time.Time { return fixedNow })
	if err != nil {
		t.Fatalf("newPluginWithClock() error = %v", err)
	}
	return p
}

func TestSignAlwaysOwnsExpirationAndUsesDefaultTTL(t *testing.T) {
	p := configuredPlugin(t, nil)
	input := Claims{
		"role": "admin",
		"exp":  jwtlib.NewNumericDate(fixedNow.Add(24 * time.Hour)),
	}
	token, err := p.Sign("alice", input)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}

	parsed := jwtlib.MapClaims{}
	if _, _, err := jwtlib.NewParser().ParseUnverified(token, parsed); err != nil {
		t.Fatalf("ParseUnverified() error = %v", err)
	}
	exp, err := parsed.GetExpirationTime()
	if err != nil || exp == nil {
		t.Fatalf("signed token exp = %v, error = %v", exp, err)
	}
	if !exp.Time.Equal(fixedNow.Add(2 * time.Hour)) {
		t.Fatalf("signed token exp = %s, want %s", exp.Time, fixedNow.Add(2*time.Hour))
	}
	if inputExp, _ := input["exp"].(*jwtlib.NumericDate); inputExp == nil || !inputExp.Time.Equal(fixedNow.Add(24*time.Hour)) {
		t.Fatal("Sign mutated the caller's claims map")
	}
}

func TestSignWithTTLRejectsNonPositiveAndWritesExpiration(t *testing.T) {
	p := configuredPlugin(t, nil)
	if _, err := p.SignWithTTL("alice", nil, 0); err == nil {
		t.Fatal("SignWithTTL accepted zero TTL")
	}
	token, err := p.SignWithTTL("alice", nil, 15*time.Minute)
	if err != nil {
		t.Fatalf("SignWithTTL() error = %v", err)
	}
	claims := jwtlib.MapClaims{}
	if _, _, err := jwtlib.NewParser().ParseUnverified(token, claims); err != nil {
		t.Fatalf("ParseUnverified() error = %v", err)
	}
	exp, _ := claims.GetExpirationTime()
	if exp == nil || !exp.Time.Equal(fixedNow.Add(15*time.Minute)) {
		t.Fatalf("exp = %v, want fixedNow + 15m", exp)
	}
}

func TestConfiguredIssuerAudienceAndLeeway(t *testing.T) {
	p := configuredPlugin(t, func(cfg *Config) {
		cfg.Issuer = "issuer.example"
		cfg.Audience = []string{"api.example"}
		cfg.Leeway = 30 * time.Second
	})
	token, err := p.Sign("alice", nil)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	runtime := p.compiled
	claims, err := runtime.verify(token)
	if err != nil {
		t.Fatalf("verify configured token: %v", err)
	}
	if claims["iss"] != "issuer.example" {
		t.Fatalf("iss = %#v", claims["iss"])
	}

	withinLeeway := jwtlib.NewWithClaims(jwtlib.SigningMethodHS256, jwtlib.MapClaims{
		"exp": jwtlib.NewNumericDate(fixedNow.Add(-15 * time.Second)),
		"iss": "issuer.example",
		"aud": "api.example",
	})
	raw, err := withinLeeway.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.verify(raw); err != nil {
		t.Fatalf("token within configured leeway rejected: %v", err)
	}

	wrongIssuer := jwtlib.NewWithClaims(jwtlib.SigningMethodHS256, jwtlib.MapClaims{
		"exp": jwtlib.NewNumericDate(fixedNow.Add(time.Hour)),
		"iss": "attacker.example",
		"aud": "api.example",
	})
	raw, err = wrongIssuer.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.verify(raw); err == nil {
		t.Fatal("token with wrong issuer was accepted")
	}

	wrongAudience := jwtlib.NewWithClaims(jwtlib.SigningMethodHS256, jwtlib.MapClaims{
		"exp": jwtlib.NewNumericDate(fixedNow.Add(time.Hour)),
		"iss": "issuer.example",
		"aud": "other.example",
	})
	raw, err = wrongAudience.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.verify(raw); err == nil {
		t.Fatal("token with wrong audience was accepted")
	}
}
