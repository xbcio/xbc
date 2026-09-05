package runtime

import (
	"fmt"
	"time"

	"github.com/xbcio/xbc/config"
)

// settingsSection is the top-level config key the framework's own knobs live
// under.
//
// It is deliberately "xbc" rather than the old "server". The core no longer
// owns a server at all -- addr / base_path / read_timeout / write_timeout
// moved out to the web module together with the thing they configure -- so
// keeping "server" would advertise a section whose two surviving members
// (shutdown timeout, auto-migrate) have nothing to do with serving anything.
// "server.*" is intentionally NOT read as a compatibility alias either: a key
// that silently keeps working while its meaning has changed underneath is
// worse than one that visibly stops being read, and the orphan-section check
// (ruling R6) already turns a stale "server:" block into a startup failure
// that names the problem outright.
const settingsSection = "xbc"

// loggingSection and applicationSection are the other two roots the runtime
// declares on somebody else's behalf: "log" belongs to the log package, and
// "app" is the one deliberately freeform root, reserved for the application
// author. Everything else at the top level must be claimed by a Definition's
// configuration path, or the configuration layer rejects it.
const (
	loggingSection     = "log"
	applicationSection = "app"
)

// settings is the core's own configuration section -- the only section the
// runtime binds for itself. Every other reserved top-level key belongs
// to somebody else: "log" to the log package, "plugins" to the assembly package,
// "app" to the application author.
//
// Both members live here rather than in any plugin because both are
// decisions only the process-level driver can make. A plugin cannot know how
// long the whole shutdown may take (its own Stop is one of many sharing that
// budget), and a plugin cannot decide whether this particular boot is allowed
// to write to a schema.
type settings struct {
	// ShutdownTimeout is the total budget for one unwind: draining managed
	// tasks and stopping every initialized plugin, together. It is a
	// whole-shutdown budget, not a per-plugin one -- see unwind (shutdown.go)
	// for why a per-plugin budget would make the worst case scale with the
	// number of plugins instead of staying bounded.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" default:"30s"`

	// AutoMigrate makes every boot run the migration stage, as if --migrate
	// had been passed. Off by default: migration is a side-effecting write,
	// and binding it to every boot means every rolling restart silently
	// touches the schema.
	AutoMigrate bool `yaml:"auto_migrate"`
}

// loadSettings binds the "xbc" section onto a zero settings and validates it.
//
// Environment.Bind applies the whole file/ENV/default chain and syncs the
// result back into the underlying koanf tree, so settings' `default` tags are
// visible to Config().Get("xbc.shutdown_timeout") afterwards, not just on the
// struct field.
func loadSettings(env *config.Environment) (settings, error) {
	var s settings
	if err := env.Bind(settingsSection, &s); err != nil {
		return s, fmt.Errorf("xbc: failed to bind %s configuration: %w", settingsSection, err)
	}
	if err := config.Validate(&s, settingsSection); err != nil {
		return s, err
	}
	if s.ShutdownTimeout <= 0 {
		return s, fmt.Errorf(
			"xbc: %s.shutdown_timeout must be positive, got %s\n  → this is the total shutdown budget; a non-positive value causes an immediate timeout and prevents graceful shutdown",
			settingsSection, s.ShutdownTimeout)
	}
	return s, nil
}
