package ratelimit

import (
	"fmt"
	"math"
)

const (
	defaultRate  = 100.0
	defaultBurst = 200
)

// Scope selects whether one token bucket is shared by all requests or one
// bucket is maintained per client IP.
type Scope string

const (
	ScopeGlobal   Scope = "global"
	ScopeClientIP Scope = "client_ip"
)

// Config is bound from plugins.ratelimit. Rate is tokens (requests) per
// second; Burst is the maximum immediately available token count.
type Config struct {
	Rate  float64 `yaml:"rate"  default:"100"    validate:"gt=0"`
	Burst int     `yaml:"burst" default:"200"    validate:"min=1"`
	Scope Scope   `yaml:"scope" default:"global" validate:"oneof=global client_ip"`
}

// DefaultConfig returns the same defaults applied by XBC's binder.
func DefaultConfig() Config {
	return Config{Rate: defaultRate, Burst: defaultBurst, Scope: ScopeGlobal}
}

// Validate checks semantic constraints, including finite rates.
func (c Config) Validate() error {
	if c.Rate <= 0 || math.IsNaN(c.Rate) || math.IsInf(c.Rate, 0) {
		return fmt.Errorf("ratelimit: rate must be a finite number greater than zero, got %v", c.Rate)
	}
	if c.Burst < 1 {
		return fmt.Errorf("ratelimit: burst must be at least 1, got %d", c.Burst)
	}
	if c.Scope != ScopeGlobal && c.Scope != ScopeClientIP {
		return fmt.Errorf("ratelimit: scope must be %q or %q, got %q", ScopeGlobal, ScopeClientIP, c.Scope)
	}
	return nil
}
