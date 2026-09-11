package web

import (
	"strings"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication"
)

func TestSecurityConfigNormalizeDefaultsToDeny(t *testing.T) {
	t.Parallel()

	got := SecurityConfig{}.normalize()
	if got.Default != SecurityDeny {
		t.Fatalf("normalize().Default = %q, want %q", got.Default, SecurityDeny)
	}
}

func TestSecurityConfigValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  SecurityConfig
		wantErr string
	}{
		{
			name: "permit and authenticate are mutually exclusive",
			config: SecurityConfig{
				Policies: []PolicyRule{{
					Match:        "/api/**",
					Permit:       true,
					Authenticate: []authentication.Scheme{"jwt"},
				}},
			},
			wantErr: "cannot combine permit with authenticate",
		},
		{
			name: "a rule must decide something",
			config: SecurityConfig{
				Policies: []PolicyRule{{Match: "/api/**"}},
			},
			wantErr: "must set either permit or authenticate",
		},
		{
			name: "empty match is rejected",
			config: SecurityConfig{
				Policies: []PolicyRule{{Match: "  ", Permit: true}},
			},
			wantErr: "empty match",
		},
		{
			name: "duplicate scheme in one rule is rejected",
			config: SecurityConfig{
				Policies: []PolicyRule{{
					Match:        "/api/**",
					Authenticate: []authentication.Scheme{"jwt", "jwt"},
				}},
			},
			wantErr: "duplicate authentication scheme",
		},
		{
			name: "invalid scheme name is rejected",
			config: SecurityConfig{
				Policies: []PolicyRule{{
					Match:        "/api/**",
					Authenticate: []authentication.Scheme{"JWT"},
				}},
			},
			wantErr: "invalid scheme",
		},
		{
			name:    "unknown default is rejected",
			config:  SecurityConfig{Default: "allow"},
			wantErr: `default must be "deny" or "permit"`,
		},
		{
			name: "valid configuration passes",
			config: SecurityConfig{
				Default: SecurityDeny,
				Policies: []PolicyRule{
					{Match: "GET /health", Permit: true},
					{Match: "/api/**", Authenticate: []authentication.Scheme{"jwt", "session"}},
				},
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.config.normalize().Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() error = nil, want substring %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %q, want substring %q", err, test.wantErr)
			}
		})
	}
}
