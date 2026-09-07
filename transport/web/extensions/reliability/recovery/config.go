package recovery

// Config is bound from plugins.recovery.
type Config struct {
	// Stack includes a runtime stack in the server-side recovery log. Panic
	// values, request headers, query strings, and bodies are never logged.
	Stack bool `yaml:"stack" default:"true"`
}

// DefaultConfig returns the same defaults applied by XBC's config binder.
func DefaultConfig() Config { return Config{Stack: true} }

// Validate exists for a uniform direct-construction API.
func (Config) Validate() error { return nil }
