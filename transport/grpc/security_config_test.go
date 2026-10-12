package grpc

import (
	"testing"

	"github.com/xbcio/xbc/extensions/authentication"
)

func TestMethodMatchSyntax(t *testing.T) {
	valid := map[string]methodMatcher{
		"/**":            {all: true},
		"/pkg.Svc/*":     {service: "pkg.Svc"},
		"/pkg.Svc/Get":   {service: "pkg.Svc", method: "Get"},
		"/a.b.c.Svc/Put": {service: "a.b.c.Svc", method: "Put"},
	}
	for raw, want := range valid {
		if got, err := parseMethodMatch(raw); err != nil || got != want {
			t.Errorf("parseMethodMatch(%q) = %+v, %v; want %+v", raw, got, err, want)
		}
	}
	for _, raw := range []string{
		"", " /**", "/** ", "pkg.Svc/Get", "/pkg.Svc", "/pkg.Svc/", "//Get", "/pkg.*/Get",
		"/pkg.Svc/Get/x", "/pkg.Svc/Ge*", "/*", "/**/Get", "/pkg Svc/Get",
	} {
		if _, err := parseMethodMatch(raw); err == nil {
			t.Errorf("parseMethodMatch(%q) accepted an unsupported form", raw)
		}
	}
}

func TestMethodMatcherCovers(t *testing.T) {
	all := methodMatcher{all: true}
	svc := methodMatcher{service: "a.S"}
	get := methodMatcher{service: "a.S", method: "Get"}
	put := methodMatcher{service: "a.S", method: "Put"}
	other := methodMatcher{service: "b.S"}
	cases := []struct {
		m, other methodMatcher
		want     bool
	}{
		{all, all, true}, {all, svc, true}, {all, get, true},
		{svc, all, false}, {svc, get, true}, {svc, other, false},
		{get, get, true}, {get, put, false}, {get, svc, false},
	}
	for _, tc := range cases {
		if got := tc.m.covers(tc.other); got != tc.want {
			t.Errorf("%+v.covers(%+v) = %v, want %v", tc.m, tc.other, got, tc.want)
		}
	}
	if !svc.matches("a.S", "Any") || svc.matches("b.S", "Any") || !get.matches("a.S", "Get") || get.matches("a.S", "Put") {
		t.Fatal("matches disagrees with the match syntax")
	}
}

func TestSecurityConfigValidation(t *testing.T) {
	invalid := map[string]SecurityConfig{
		"unknown default":       {Default: "allow"},
		"invalid scheme":        {Default: SecurityDeny, Schemes: []authentication.Scheme{"Bad"}},
		"empty match":           {Default: SecurityDeny, Rules: []SecurityRule{{Match: " ", Permit: true}}},
		"permit and schemes":    {Default: SecurityDeny, Rules: []SecurityRule{{Match: "/**", Permit: true, Authenticate: []authentication.Scheme{"jwt"}}}},
		"decides nothing":       {Default: SecurityDeny, Rules: []SecurityRule{{Match: "/**"}}},
		"bad match":             {Default: SecurityDeny, Rules: []SecurityRule{{Match: "/a.S/G*", Permit: true}}},
		"invalid rule scheme":   {Default: SecurityDeny, Rules: []SecurityRule{{Match: "/**", Authenticate: []authentication.Scheme{"JWT"}}}},
		"duplicate rule scheme": {Default: SecurityDeny, Rules: []SecurityRule{{Match: "/**", Authenticate: []authentication.Scheme{"jwt", "jwt"}}}},
	}
	for name, cfg := range invalid {
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, cfg)
		}
	}
	if (SecurityConfig{}).normalize().Default != SecurityDeny {
		t.Fatal("an omitted default must normalize to deny")
	}
	original := SecurityConfig{Rules: []SecurityRule{{Match: "/**", Authenticate: []authentication.Scheme{"jwt"}}}}
	normalized := original.normalize()
	normalized.Rules[0].Authenticate[0] = "mutated"
	if original.Rules[0].Authenticate[0] != "jwt" {
		t.Fatal("normalize must detach the caller's slices")
	}
}
