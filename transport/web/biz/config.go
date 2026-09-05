package biz

// Config is the business response contract configuration. The contract is
// intentionally parameter-free: composing biz.Bundle is the complete opt-in
// and avoids making wire field names or success semantics environment-dependent.
type Config struct{}

// DefaultConfig returns the parameter-free business contract configuration.
func DefaultConfig() Config { return Config{} }
