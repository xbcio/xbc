package ratelimit

import (
	"math"
	"testing"

	"github.com/xbcio/xbc/config"
)

func TestConfigDefaultsAndValidation(t *testing.T) {
	env, err := config.NewEnvironment(nil, "XBC_RATELIMIT_TEST_")
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := env.Bind("plugins.ratelimit", &cfg); err != nil {
		t.Fatal(err)
	}
	if err := config.Validate(&cfg, "plugins.ratelimit"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg != DefaultConfig() {
		t.Fatalf("defaults = %+v, want %+v", cfg, DefaultConfig())
	}
}

func TestConfigRejectsInvalidValues(t *testing.T) {
	for _, cfg := range []Config{
		{Rate: 0, Burst: 1, Scope: ScopeGlobal},
		{Rate: -1, Burst: 1, Scope: ScopeGlobal},
		{Rate: math.NaN(), Burst: 1, Scope: ScopeGlobal},
		{Rate: math.Inf(1), Burst: 1, Scope: ScopeGlobal},
		{Rate: 1, Burst: 0, Scope: ScopeGlobal},
		{Rate: 1, Burst: 1, Scope: "user"},
	} {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("Validate(%+v) succeeded, want error", cfg)
		}
	}
}
