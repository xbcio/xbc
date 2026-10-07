// Package config loads layered application configuration and strictly binds
// typed YAML schemas. Runtime consumers should depend on View, optionally
// narrowed with Scope; mutation through Environment.Bind is reserved for
// assembly code that owns an Environment.
//
// # Loading
//
// Load searches the current working directory for application.yml and then
// configs/application.yml unless Options.File names an explicit file. An
// explicit file must exist. Options.Profile adds an optional sibling overlay:
// profile "prod" turns application.yml into application-prod.yml.
//
// The effective precedence, from lowest to highest, is a struct default tag,
// which fills only a field the caller left at its zero value; the non-zero
// values a caller pre-filled for a typed target (the Config a plugin's
// ConfigSpec.Defaults returns, typically); the base YAML file; the profile
// file; Options.Overrides; and environment variables. A pre-filled non-zero
// value therefore wins over a default tag. YAML strings are literal; the
// loader does not expand ${VAR}.
// NewEnvironment bypasses files and process environment for callers that
// already own an in-memory configuration tree.
//
// # Ownership and binding
//
// A Universe declares every valid configuration section and whether it is
// typed, instanced, free-form, or a namespace. When Options.Universe is set,
// Load rejects paths that no section owns. Environment.Bind applies defaults,
// performs strict YAML-name binding, validates struct tags, and rejects unknown
// fields. Environment implements View; Get, Exists, Sub, and Bind all read the
// same merged tree.
//
// A map or slice the configuration supplies replaces the value the caller
// pre-filled instead of merging into it, so a user map is exactly the keys it
// writes and a decoded struct slice does not inherit pre-filled element
// fields. Scalars keep their pre-filled values where the configuration is
// silent. Scalars are also not coerced: a fractional float for an integer
// field, a quoted number for an integer field, or a bool for a string field is
// an error rather than a truncated or rewritten value, and so is a scalar
// where a list field is expected -- skip_paths: /healthz is an error, not a
// one-element list; write lists as lists. A type that defines its own text
// spelling is the exception: encoding.TextUnmarshaler fields receive
// text, including the decimal text of an unquoted integer, and durations are
// parsed from their usual spellings.
//
// # Optional subtrees
//
// A pointer sub-struct stays nil until the file, an environment variable, or a
// default tag inside it activates the block. That last case is a trap for
// schema authors: a default tag inside a *Sub makes Bind allocate the block
// even when the user configured nothing, so a required field inside it fails
// for every user who left the optional block out. Do not put required
// validation inside a pointer sub-struct that carries default tags; validate
// the block conditionally in Prepare after checking whether it was set.
//
// Leaving Options.Universe nil is an embedding escape hatch: Load still merges
// files and Overrides, but it cannot resolve an environment layer or validate
// section ownership without the declared schemas.
//
// # Environment variables
//
// DefaultEnvPrefix is XBC_. A variable name is derived from the complete
// configuration path by replacing dots and hyphens with underscores and
// converting it to uppercase. For example, web.addr maps to XBC_WEB_ADDR and
// plugins.redis.cache.password maps to XBC_PLUGINS_REDIS_CACHE_PASSWORD.
//
// A section whose root already spells the process prefix does not repeat it.
// The framework's own section is rooted at "xbc", so xbc.shutdown_timeout maps
// to XBC_SHUTDOWN_TIMEOUT and xbc.runtime.max_procs maps to
// XBC_RUNTIME_MAX_PROCS, never to the doubled XBC_XBC_* form. The test is on a
// whole path segment rather than on a string prefix, so an unrelated section
// such as xbcx keeps its complete spelling, XBC_XBCX_*. Instanced sections
// discover the instance name between their section prefix and field suffix.
//
// Resolution is schema-aware rather than a blind split on underscores, so
// names such as base_path remain unambiguous. Scalars, durations, values that
// implement encoding.TextUnmarshaler, and string slices can be supplied by one
// variable. Shapes that cannot be represented by one variable fail explicitly.
// An instanced section discovers an instance name in the variable, but a
// declared section or leaf whose own spelling matches the variable wins over
// that inference, so XBC_PLUGINS_REDIS_LEASE_ENABLED toggles a declared
// plugins.redis-lease rather than instance "lease" of plugins.redis.
// Environment variables form the highest-precedence layer and may create a
// selected section or named instance even when it is absent from YAML. A
// variable that names no declared section, or no field of the section it names,
// is an error rather than a silent no-op.
//
// # Sensitive fields
//
// A field tagged mask:"true" is redacted in validation diagnostics. The mask
// is inherited by nested fields, and the tag value must be exactly "true" or
// "false". Masking changes diagnostics only; applications should still obtain
// secrets from deployment environment variables or a dedicated secret
// provider rather than committing them to configuration files.
package config
