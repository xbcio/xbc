package casbin

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultConfigIsFailClosed(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.RequestConvention != ConventionRoutePermission {
		t.Fatalf("RequestConvention = %q, want %q", cfg.RequestConvention, ConventionRoutePermission)
	}
	if cfg.MissingPermission != MissingPermissionDeny {
		t.Fatalf("MissingPermission = %q, want %q", cfg.MissingPermission, MissingPermissionDeny)
	}
	if cfg.ReloadInterval != 0 {
		t.Fatalf("ReloadInterval = %s, want disabled", cfg.ReloadInterval)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() defaults error = %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "inline model", mutate: func(c *Config) { c.Model = permissionModel }},
		{name: "inline policy", mutate: func(c *Config) { c.Policy = "p, alice, reports:read" }},
		{name: "path method", mutate: func(c *Config) { c.RequestConvention = ConventionPathMethod }},
		{name: "explicit missing permission allow", mutate: func(c *Config) { c.MissingPermission = MissingPermissionAllow }},
		{name: "unknown request convention", mutate: func(c *Config) { c.RequestConvention = "resource" }, wantErr: "request_convention"},
		{name: "unknown missing policy", mutate: func(c *Config) { c.MissingPermission = "ignore" }, wantErr: "missing_permission"},
		{name: "model source conflict", mutate: func(c *Config) { c.Model, c.ModelFile = permissionModel, "model.conf" }, wantErr: "mutually exclusive"},
		{name: "policy source conflict", mutate: func(c *Config) { c.Policy, c.PolicyFile = "p, alice, read", "policy.csv" }, wantErr: "mutually exclusive"},
		{name: "negative interval", mutate: func(c *Config) { c.ReloadInterval = -time.Second }, wantErr: "cannot be negative"},
		{name: "reload without file", mutate: func(c *Config) { c.ReloadInterval = time.Second }, wantErr: "requires policy_file"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}
