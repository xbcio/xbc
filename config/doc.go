// Package config loads layered application configuration and strictly binds
// typed YAML schemas. Runtime consumers should depend on View (optionally
// narrowed with Scope); mutation through Bind is reserved for assembly code
// that owns an Environment.
package config
