package config

// Option configures New. Each option sets one Options field, so
// New(WithFile(p), WithEnvFile(e)) is the same as Load(Options{File: p,
// EnvFile: e}); later options override earlier ones.
type Option func(*Options)

// New merges the configured sources and returns the Environment. It is the
// functional-option form of Load.
func New(opts ...Option) (*Environment, error) {
	var options Options
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	return Load(options)
}

// WithFile names the main configuration file, which must then exist.
func WithFile(path string) Option { return func(o *Options) { o.File = path } }

// WithFiles appends further configuration files merged after the main file, in
// order; a missing one is skipped.
func WithFiles(paths ...string) Option {
	return func(o *Options) { o.Files = append(o.Files, paths...) }
}

// WithProfile selects the profile overlay file next to the main file.
func WithProfile(profile string) Option { return func(o *Options) { o.Profile = profile } }

// WithEnvPrefix sets the prefix of the schema-driven environment layer.
func WithEnvPrefix(prefix string) Option { return func(o *Options) { o.EnvPrefix = prefix } }

// WithEnvFile names a dotenv file feeding the environment layer below the
// process environment.
func WithEnvFile(path string) Option { return func(o *Options) { o.EnvFile = path } }

// WithEnvKeys sets the rule mapping environment variable names to
// configuration paths for a caller without a Universe.
func WithEnvKeys(fn func(name string) (path string, ok bool)) Option {
	return func(o *Options) { o.EnvKeys = fn }
}

// WithDefaults sets the lowest-precedence layer.
func WithDefaults(defaults Defaults) Option { return func(o *Options) { o.Defaults = defaults } }

// WithOverrides sets flat dotted-path values merged between the files and the
// environment layer.
func WithOverrides(values map[string]any) Option { return func(o *Options) { o.Overrides = values } }

// WithUniverse declares who owns which configuration path.
func WithUniverse(universe *Universe) Option { return func(o *Options) { o.Universe = universe } }

// WithoutFile disables the lookup of application.yml, so the tree is built only
// from the defaults, overrides and environment layers.
func WithoutFile() Option { return func(o *Options) { o.NoFile = true } }
