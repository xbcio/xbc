package casbin

import (
	"strings"
	"testing"
)

func TestDefaultConfigIsFailClosed(t *testing.T) {
	if got := DefaultConfig().MissingPermission; got != MissingPermissionDeny {
		t.Fatalf("DefaultConfig().MissingPermission = %q, want deny", got)
	}
}

func TestConfigValidation(t *testing.T) {
	cases := map[string]struct {
		cfg  Config
		want string
	}{
		"empty means deny": {
			cfg: Config{},
		},
		"explicit deny": {
			cfg: Config{MissingPermission: MissingPermissionDeny},
		},
		"explicit allow": {
			cfg: Config{MissingPermission: MissingPermissionAllow},
		},
		"unknown policy": {
			cfg:  Config{MissingPermission: "skip"},
			want: `must be "deny" or "allow"`,
		},
		"wrong case": {
			cfg:  Config{MissingPermission: "Deny"},
			want: `must be "deny" or "allow"`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() error = %v, want it to contain %q", err, tc.want)
			}
			if _, err := prepareConfig(tc.cfg); err == nil {
				t.Fatal("prepareConfig accepted an invalid config")
			}
		})
	}
}
