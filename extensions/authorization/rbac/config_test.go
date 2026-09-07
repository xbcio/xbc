package rbac

import (
	"strings"
	"testing"

	"github.com/xbcio/xbc/plugin"
)

func TestDefaultConfigSelectsDefaultCasbinAndAdminRole(t *testing.T) {
	got := DefaultConfig()
	if got.Backend.Plugin != "casbin" || got.Backend.Instance != plugin.DefaultInstance || got.AdminRole != "admin" {
		t.Fatalf("DefaultConfig() = %#v", got)
	}
	if err := (Config{}).Validate(); err != nil {
		t.Fatalf("zero Config should normalize to defaults: %v", err)
	}
}

func TestConfigNormalizesAdminRoleAndDefaultBackendFields(t *testing.T) {
	normalized, err := normalizeConfig(Config{
		Backend:   ProviderRef{Instance: "policy"},
		AdminRole: "  super-admin  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if normalized.backend != (plugin.Identity{Plugin: "casbin", Instance: "policy"}) {
		t.Fatalf("backend = %#v", normalized.backend)
	}
	if normalized.adminRole != "super-admin" {
		t.Fatalf("admin role = %q", normalized.adminRole)
	}
}

func TestConfigRejectsInvalidProviderReferencesAndAdminRole(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr string
	}{
		{name: "plugin whitespace", config: Config{Backend: ProviderRef{Plugin: " casbin"}}, wantErr: "backend.plugin"},
		{name: "invalid plugin", config: Config{Backend: ProviderRef{Plugin: "Casbin"}}, wantErr: "backend.plugin"},
		{name: "instance whitespace", config: Config{Backend: ProviderRef{Plugin: "casbin", Instance: "policy "}}, wantErr: "backend.instance"},
		{name: "invalid instance", config: Config{Backend: ProviderRef{Plugin: "casbin", Instance: "Policy DB"}}, wantErr: "backend.instance"},
		{name: "empty admin role", config: Config{AdminRole: " \t "}, wantErr: "admin_role"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.config.Validate()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}
