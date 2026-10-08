package casbin

import (
	"strings"
	"testing"
)

func TestDefaultConfigIsFailClosed(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.MissingPermission != MissingPermissionDeny {
		t.Fatalf("MissingPermission = %q, want %q", cfg.MissingPermission, MissingPermissionDeny)
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
		{name: "zero value normalizes to deny"},
		{name: "explicit missing permission allow", mutate: func(c *Config) { c.MissingPermission = MissingPermissionAllow }},
		{name: "unknown missing policy", mutate: func(c *Config) { c.MissingPermission = "ignore" }, wantErr: "missing_permission"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{}
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}
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
