package tenant

import (
	"context"
	"errors"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

func defaultPrincipalResolver(t *testing.T) principalResolver {
	t.Helper()
	cfg, err := normalizeConfig(defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return principalResolver{cfg: cfg}
}

func TestPrincipalResolverSelectsOnlyVerifiedMemberships(t *testing.T) {
	resolver := defaultPrincipalResolver(t)
	principal := web.Principal{
		Subject: "alice",
		Attributes: map[string]any{
			"tenant_id":  "primary",
			"tenant_ids": []any{"secondary", "primary"},
			"tenant_attributes": map[string]any{
				"secondary": map[string]any{"plan": "enterprise", "nested": map[string]any{"region": "cn"}},
			},
		},
	}
	selected, found, err := resolver.ResolveTenant(context.Background(), principal, "secondary")
	if err != nil || !found || selected.ID != "secondary" || selected.Attributes["plan"] != "enterprise" {
		t.Fatalf("selected=%#v found=%v err=%v", selected, found, err)
	}
	selected.Attributes["nested"].(map[string]any)["region"] = "mutated"
	original := principal.Attributes["tenant_attributes"].(map[string]any)["secondary"].(map[string]any)
	if original["nested"].(map[string]any)["region"] != "cn" {
		t.Fatal("resolver result aliases principal attributes")
	}
	if _, found, err := resolver.ResolveTenant(context.Background(), principal, "attacker"); err != nil || found {
		t.Fatalf("unverified requested ID found=%v err=%v", found, err)
	}
	if _, found, err := resolver.ResolveTenant(context.Background(), principal, " secondary"); err != nil || found {
		t.Fatalf("whitespace selector found=%v err=%v", found, err)
	}
	if _, found, err := resolver.ResolveTenant(context.Background(), principal, ""); err != nil || found {
		t.Fatalf("multiple memberships auto-selected: found=%v err=%v", found, err)
	}
}

func TestPrincipalResolverAutoSelectsExactlyOneTenant(t *testing.T) {
	resolver := defaultPrincipalResolver(t)
	for name, attributes := range map[string]map[string]any{
		"single": {"tenant_id": "acme"},
		"list":   {"tenant_ids": []string{"acme"}},
	} {
		t.Run(name, func(t *testing.T) {
			resolved, found, err := resolver.ResolveTenant(context.Background(), web.Principal{Subject: "alice", Attributes: attributes}, "")
			if err != nil || !found || resolved.ID != "acme" {
				t.Fatalf("resolved=%#v found=%v err=%v", resolved, found, err)
			}
		})
	}

	cfg := defaultConfig()
	cfg.AutoSelectSingle = false
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	withoutAuto := principalResolver{cfg: normalized}
	if _, found, err := withoutAuto.ResolveTenant(context.Background(), web.Principal{Subject: "alice", Attributes: map[string]any{"tenant_id": "acme"}}, ""); err != nil || found {
		t.Fatalf("AutoSelectSingle=false found=%v err=%v", found, err)
	}
}

func TestPrincipalResolverRejectsMalformedVerifiedFactsAndCancellation(t *testing.T) {
	resolver := defaultPrincipalResolver(t)
	cases := map[string]map[string]any{
		"wrong scalar type": {"tenant_id": 123},
		"wrong list type":   {"tenant_ids": "acme"},
		"mixed list":        {"tenant_ids": []any{"acme", 7}},
		"empty member":      {"tenant_ids": []string{""}},
		"unsafe member":     {"tenant_id": "../../admin"},
		"oversized member":  {"tenant_id": string(make([]byte, 129))},
	}
	for name, attributes := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := resolver.ResolveTenant(context.Background(), web.Principal{Subject: "alice", Attributes: attributes}, ""); err == nil {
				t.Fatal("malformed principal facts accepted")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := resolver.ResolveTenant(ctx, web.Principal{Subject: "alice"}, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ResolveTenant error=%v", err)
	}
	if _, _, err := resolver.ResolveTenant(nil, web.Principal{Subject: "alice"}, ""); err == nil {
		t.Fatal("nil context accepted")
	}
	var nilResolver ResolverFunc
	if _, _, err := nilResolver.ResolveTenant(context.Background(), web.Principal{Subject: "alice"}, ""); err == nil {
		t.Fatal("nil ResolverFunc accepted")
	}
}

func TestSetAndCurrentValidateAndDefensivelyCopy(t *testing.T) {
	ctx, _ := gin.CreateTestContext(nil)
	input := Tenant{ID: "acme", Attributes: map[string]any{
		"nested": map[string]any{"region": "cn"},
		"roles":  []string{"reader"},
		"bytes":  []byte("secret"),
	}}
	if !Set(ctx, input) {
		t.Fatal("Set rejected a valid trusted tenant")
	}
	input.Attributes["nested"].(map[string]any)["region"] = "input-mutated"
	input.Attributes["roles"].([]string)[0] = "writer"
	input.Attributes["bytes"].([]byte)[0] = 'X'
	current, ok := Current(ctx)
	if !ok || current.ID != "acme" || current.Attributes["nested"].(map[string]any)["region"] != "cn" || current.Attributes["roles"].([]string)[0] != "reader" || string(current.Attributes["bytes"].([]byte)) != "secret" {
		t.Fatalf("Current=%#v ok=%v", current, ok)
	}
	current.Attributes["nested"].(map[string]any)["region"] = "output-mutated"
	again, _ := Current(ctx)
	if again.Attributes["nested"].(map[string]any)["region"] != "cn" {
		t.Fatal("Current returned aliased attributes")
	}
	for _, invalid := range []string{"", " tenant", "../admin", "tenant,other", string(make([]byte, 129))} {
		if Set(ctx, Tenant{ID: invalid}) {
			t.Fatalf("Set accepted invalid ID %q", invalid)
		}
	}
	if _, ok := Current(nil); ok || Set(nil, Tenant{ID: "acme"}) {
		t.Fatal("nil Gin context was accepted")
	}
}
