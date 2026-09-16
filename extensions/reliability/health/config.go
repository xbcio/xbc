package health

import (
	"fmt"
	"time"
)

// Config is bound from plugins.health. It carries only protocol-neutral probe
// policy: a transport adapter owns its own endpoint paths and its own rule for
// whether diagnostic errors may cross the protocol boundary.
type Config struct {
	Timeout time.Duration `yaml:"timeout" default:"2s" validate:"gt=0"`
}

// DefaultConfig returns the same production-safe defaults applied by XBC's
// configuration binder.
func DefaultConfig() Config {
	return Config{Timeout: 2 * time.Second}
}

func (c Config) validate() error {
	if c.Timeout <= 0 {
		return fmt.Errorf("health: timeout must be greater than zero")
	}
	return nil
}
