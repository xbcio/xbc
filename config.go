// config.go
package xbc

import (
	"fmt"
	"reflect"
	"time"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"

	"github.com/xbcio/xbc/internal/conf"
	"github.com/xbcio/xbc/log"
)

const defaultEnvPrefix = "XBC_"

// Config is the root configuration surface handed to every plugin via
// Context.Config(). Server and Log are bound eagerly during stage 1;
// everything else (plugins.*, app.*) is reached through k via Get/Exists/Sub.
type Config struct {
	Server ServerConfig `yaml:"server"`
	Log    log.Config   `yaml:"log"`

	k *koanf.Koanf
}

// ServerConfig configures the HTTP server assembled in stage 7.
//
// WriteTimeout is not in the spec's illustrative §6.2 snippet -- it is added
// by this plan (ruling R10). A read-only timeout leaves the write side open
// to a slow-write style attack surface.
type ServerConfig struct {
	Addr            string        `yaml:"addr"             default:":8080"`
	BasePath        string        `yaml:"base_path"        default:"/"`
	ReadTimeout     time.Duration `yaml:"read_timeout"     default:"10s"`
	WriteTimeout    time.Duration `yaml:"write_timeout"    default:"30s"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" default:"30s"`
	AutoMigrate     bool          `yaml:"auto_migrate"`
}

// Unmarshal binds a config subtree into out, applying the full stage-3 chain
// (unmarshal -> ENV -> default). It does NOT run validate -- callers that
// need validation call conf.Validate separately (see bindConfigs in
// stage_config.go, which does exactly that for every plugin instance).
func (c *Config) Unmarshal(path string, out any) error {
	if c == nil || c.k == nil {
		return fmt.Errorf("xbc: 配置尚未加载，无法绑定 %s", displayConfigPath(path))
	}
	return conf.Bind(c.k, path, out, defaultEnvPrefix)
}

func displayConfigPath(path string) string {
	if path == "" {
		return "(root)"
	}
	return path
}

// Get returns the raw value at path, or nil when absent.
func (c *Config) Get(path string) any {
	if c == nil || c.k == nil {
		return nil
	}
	return c.k.Get(path)
}

// Exists reports whether path was set by any of the loaded layers.
//
// It answers two different questions depending on which section path falls
// under, because loadConfig's syncBack (below) only ever runs against
// server/log:
//
//   - Under "server" / "log" (the framework's own typed schema): Exists
//     answers "does this config item exist" -- effectively always true for
//     any leaf on the schema, because syncBack writes every leaf's final
//     value back into k, including leaves that have no `default` tag and
//     that the user never set (they land as their Go zero value, e.g.
//     ServerConfig.AutoMigrate defaults to false and Exists("server.auto_migrate")
//     is true regardless of whether anyone wrote it).
//   - Under "app.*" / "plugins.*" (no schema, freeform): Exists answers "did
//     the user actually set this" -- true only when the file/ENV/Overrides
//     layers actually wrote it, because nothing ever syncs those sections
//     back into k the way server/log are synced.
//
// Pinned consequence for later tasks: anything that needs to tell "the user
// configured this" apart from "the framework filled this in with a zero or
// default value" -- a config doctor command, a config echo/dump endpoint,
// etc. -- must NOT use Exists for that purpose on the server/log sections.
// Doing so would report every schema leaf as user-configured. Making that
// distinction properly would require internal/conf's Leaf to also carry
// whether a `default` tag exists and whether ENV/file actually hit it, and
// syncBack to only write back the leaves that were actually set -- a change
// to internal/conf's exported API this design deliberately did not make.
func (c *Config) Exists(path string) bool {
	if c == nil || c.k == nil {
		return false
	}
	return c.k.Exists(path)
}

// Sub returns the map at path, or nil when path is absent or not a map.
func (c *Config) Sub(path string) map[string]any {
	if c == nil || c.k == nil {
		return nil
	}
	m, ok := c.k.Get(path).(map[string]any)
	if !ok {
		return nil
	}
	return m
}

// syncBack merges out's fully-bound leaf values back into k under path, so
// that Config.Get/Exists/Sub -- which only ever read k, never out -- see the
// same ENV overlays and `default` tags that Bind just applied directly onto
// out's fields. Without this, a subtree built entirely from default tags
// (no config file, no ENV) would be invisible to Exists/Get/Sub even though
// it is very much part of the merged configuration Config promises to
// expose.
//
// This walks the same Leaves schema Bind itself used, so the two agree on
// what a "leaf" is; confmap.Provider with "." as the delimiter is the same
// mechanism conf.Load already uses to merge Options.Overrides into k.
func syncBack(k *koanf.Koanf, path string, out any) error {
	leaves := conf.Leaves(path, out)
	if len(leaves) == 0 {
		return nil
	}

	v := reflect.ValueOf(out)
	for v.Kind() == reflect.Pointer {
		v = v.Elem()
	}

	flat := make(map[string]any, len(leaves))
	for _, leaf := range leaves {
		flat[leaf.Path] = v.FieldByIndex(leaf.Index).Interface()
	}
	return k.Load(confmap.Provider(flat, "."), nil)
}

// bindLog binds the "log" subtree onto a fresh log.DefaultConfig(), wires it
// into the global logger, and hands the bound config back so loadConfig can
// keep it on Config.Log for anything that wants to read what log is running.
//
// Ordering constraint: Bind must run BEFORE log.Init. Bind is the only step
// that applies the file/ENV/default chain (including ruling R2's schema-driven
// ENV overlay); log.Init only normalizes and assembles zap cores from
// whatever Config it is handed -- it has no idea a "log" config section, ENV
// vars, or defaults even exist. Calling log.Init first and Bind second would
// silently discard every override Bind was supposed to apply.
func bindLog(k *koanf.Koanf, envPrefix string) (log.Config, error) {
	cfg := log.DefaultConfig()
	if err := conf.Bind(k, "log", &cfg, envPrefix); err != nil {
		return cfg, fmt.Errorf("xbc: 绑定 log 配置失败：%w", err)
	}
	if err := syncBack(k, "log", &cfg); err != nil {
		return cfg, fmt.Errorf("xbc: 回写 log 配置失败：%w", err)
	}
	if err := log.Init(cfg); err != nil {
		return cfg, fmt.Errorf("xbc: 初始化日志失败：%w", err)
	}
	return cfg, nil
}

// loadConfig is assembly stage 1: locate and merge the config sources, bring
// the logger up, bind and validate the framework's own server section, and
// park the result on App for the nine stages that follow.
//
// Log is bound before server, but as of this task that ordering does not
// actually change any observable behavior: neither Bind nor Validate emits a
// log line on their error paths today, so swapping the two blocks leaves
// every test green (verified by hand, not just asserted). The ordering is
// kept log-first anyway so that the moment a later change makes the server
// section's error paths want to log something, it already has a working
// logger to log through -- nobody has to remember to reorder these two
// blocks first. If that day comes, add a test that actually observes a log
// line here; until then a test asserting on ordering alone would just be a
// tautology dressed up as coverage.
func (a *App) loadConfig(opts conf.Options) error {
	if opts.EnvPrefix == "" {
		opts.EnvPrefix = defaultEnvPrefix
	}
	k, err := conf.Load(opts)
	if err != nil {
		return err
	}

	cfg := &Config{k: k}
	if cfg.Log, err = bindLog(k, opts.EnvPrefix); err != nil {
		return err
	}
	if err := conf.Bind(k, "server", &cfg.Server, opts.EnvPrefix); err != nil {
		return fmt.Errorf("xbc: 绑定 server 配置失败：%w", err)
	}
	if err := syncBack(k, "server", &cfg.Server); err != nil {
		return fmt.Errorf("xbc: 回写 server 配置失败：%w", err)
	}
	if err := conf.Validate(&cfg.Server, "server"); err != nil {
		return err
	}

	a.cfg = cfg
	return nil
}
