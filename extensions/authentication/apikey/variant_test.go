package apikey

import (
	"context"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication"
)

func TestWithSchemeSetsTheInstanceScheme(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	cfg := DefaultConfig()
	cfg.Static = []StaticCredential{{Subject: "service:mcp", SHA256: HashKey(secret).String()}}

	def, err := New(cfg)
	if err != nil || def.Scheme() != Scheme {
		t.Fatalf("default Scheme() = %q, err=%v; want %q", def.Scheme(), err, Scheme)
	}
	variant, err := New(cfg, WithScheme("mcp-token"))
	if err != nil {
		t.Fatalf("New(WithScheme) error = %v", err)
	}
	if variant.Scheme() != "mcp-token" {
		t.Fatalf("Scheme() = %q", variant.Scheme())
	}
	credential, _ := authentication.Presented(PresentedKey{Secret: secret}).Credential()
	result, err := variant.Authenticate(context.Background(), credential)
	if err != nil || !result.Authenticated() {
		t.Fatalf("Authenticate() = %+v, %v", result, err)
	}
	if principal, _ := result.Principal(); principal.(authentication.Principal).AuthMethod != "mcp-token" {
		t.Fatalf("AuthMethod = %v", principal)
	}
}

func TestWithSchemeRejectsInvalidValues(t *testing.T) {
	for _, scheme := range []authentication.Scheme{"", "MCP", "a b"} {
		if _, err := New(DefaultConfig(), WithScheme(scheme)); err == nil {
			t.Fatalf("New(WithScheme(%q)) succeeded", scheme)
		}
	}
}

func TestIgnoreAppIDConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Static = []StaticCredential{{Subject: "s", SHA256: HashKey("0123456789abcdef0123456789abcdef").String()}}
	cfg.IgnoreAppID = true
	if p, err := New(cfg); err != nil || !p.IgnoreAppID() {
		t.Fatalf("IgnoreAppID() = %v, err=%v", p != nil && p.IgnoreAppID(), err)
	}
	cfg.RequireAppID = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("ignore_app_id with require_app_id was accepted")
	}
	if DefaultConfig().IgnoreAppID {
		t.Fatal("IgnoreAppID must default to false")
	}
}
