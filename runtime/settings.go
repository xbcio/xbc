package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
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
// worse than one that visibly stops being read, and a stale "server:" block is
// now owned by nobody, so the ownership check turns it into a startup failure
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
// Every member lives here rather than in any plugin because each is a
// decision only the process-level driver can make. A plugin cannot know how
// long the whole shutdown may take (its own Stop is one of many sharing that
// budget), a plugin cannot decide whether this particular boot is allowed to
// write to a schema, and no plugin can see the whole startup it may be the one
// holding up.
type settings struct {
	// ShutdownTimeout is the total budget for one unwind: draining managed
	// tasks and stopping every initialized plugin, together. It is a
	// whole-shutdown budget, not a per-plugin one -- see unwind (shutdown.go)
	// for why a per-plugin budget would make the worst case scale with the
	// number of plugins instead of staying bounded.
	//
	// It is the second of a stop's two budgets. PreStopTimeout runs first and
	// is accounted separately, so the worst case for one stop is
	// PreStopTimeout + ShutdownTimeout (2s + 30s by default). Keeping them
	// apart is what lets a deployment that needs a long lease-release window
	// widen that window without handing every Stop a budget it does not need.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" default:"30s"`

	// PreStopTimeout is the total budget for the pre-stop phase: every plugin's
	// PreStop runs inside this one budget, exactly as every plugin's Stop runs
	// inside ShutdownTimeout, and for the same reason -- a per-plugin budget
	// would make the worst case scale with the number of plugins instead of
	// staying bounded.
	//
	// It is its own knob because the phase does different work under different
	// constraints from Stop. PreStop retracts a value's externally visible
	// participation while the process is still fully alive, so its remote calls
	// need a real budget, whereas Stop runs against an already-cancelled
	// execution context and mostly closes things locally.
	//
	// 0s skips the phase entirely: no PreStop hook is called. That is the
	// documented off switch, and it is what an application that declares no
	// PreStop anywhere is doing implicitly. Negative is not an off switch, it is
	// a mistake, and loadSettings rejects it.
	PreStopTimeout time.Duration `yaml:"pre_stop_timeout" default:"2s"`

	// AutoMigrate makes every boot run the migration stage, as if --migrate
	// had been passed. Off by default: migration is a side-effecting write,
	// and binding it to every boot means every rolling restart silently
	// touches the schema.
	AutoMigrate bool `yaml:"auto_migrate"`

	// SlowStartupAfter is how long a startup may run before the runtime
	// reports where it currently is, and how often it repeats that report
	// until startup finishes. It reports only: it never cancels, aborts, or
	// times out a startup, and no deadline is derived from it.
	//
	// 0s turns the report off. The default is deliberately a positive value
	// rather than 0s, because the failure this exists for -- a plugin hook
	// that never returns -- is silent under the default configuration
	// otherwise: the released-gate line and the per-plugin timing breakdown
	// are both emitted after every phase has returned, so on the one boot
	// that never finishes neither of them is ever reached.
	//
	// An application whose migrations legitimately run for minutes should
	// raise this or set it to 0s; otherwise every such boot warns about a
	// startup that is doing exactly what it was asked to do.
	SlowStartupAfter time.Duration `yaml:"slow_startup_after" default:"30s" validate:"gte=0"`

	// Runtime holds the process-level knobs the framework owns on behalf of
	// every plugin it may load. They live under "xbc" rather than under any
	// plugin's own section because no plugin can own a decision that applies
	// to the whole process -- see runtimeKnobs (runtime_knobs.go).
	Runtime runtimeSettings `yaml:"runtime"`

	// InstanceID names this process. It is the identity a placement source
	// records as the holder of whatever it claims, and the label an operator
	// correlates a log line, a metric and a `doctor` report by.
	//
	// Empty means "derive one": the runtime composes a token from the
	// hostname, the boot time and a random suffix, so that two processes
	// started on the same host are distinguishable without any deployment
	// input. A deployment that assigns the identity itself -- because its
	// supervisor already knows the slot -- sets this instead, and the value is
	// then used exactly as written.
	//
	// It is deliberately not unique-per-boot when set explicitly: an operator
	// who names a process means that name to survive a restart.
	InstanceID string `yaml:"instance_id"`

	// instanceID is InstanceID with the derived default applied, resolved once
	// while binding. It is unexported and carries no yaml tag, so the
	// configuration schema never sees it: it is not a configuration key, it is
	// the answer this process settled on for one.
	//
	// Resolving it once here rather than at each read is what makes it usable
	// as an identity. A token recomputed per call would change between the
	// lease acquisition that recorded it and the renewal that has to present
	// it, and the process would then fail to renew the very lease it holds.
	instanceID string
}

// Instance returns this process's stable identity: the configured instance_id
// when one was given, and the derived default otherwise.
func (s settings) Instance() string { return s.instanceID }

// deriveInstanceID composes the default identity: the hostname, the boot time
// and a random suffix, in a log-friendly form such as
// "host-7-1758091200-9f3c1a2b".
//
// All three parts earn their place. The hostname answers "which machine"; the
// boot time orders two processes that ever shared a host and makes a stale
// lease holder recognizable in a log; the random suffix separates two
// processes that started in the same second, which a restart loop does
// routinely.
//
// It does not fail when the hostname is unavailable: an unnamed host is a
// deployment detail, not a reason to refuse to start, and the remaining two
// parts still identify the process within any window an operator can observe.
func deriveInstanceID(now time.Time) (string, error) {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown-host"
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("xbc: cannot derive %s.instance_id: %w", settingsSection, err)
	}
	return fmt.Sprintf("%s-%d-%s", sanitizeHostname(host), now.Unix(), hex.EncodeToString(suffix)), nil
}

// sanitizeHostname reduces a hostname to the characters an identity token may
// carry. It lowercases and replaces anything outside [a-z0-9-] with a hyphen,
// so a fully qualified or oddly punctuated host still produces one token that
// reads as one token wherever it is printed.
func sanitizeHostname(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	var b strings.Builder
	b.Grow(len(host))
	lastHyphen := false
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		case !lastHyphen && b.Len() > 0:
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		return "unknown-host"
	}
	return name
}

// runtimeSettings is the "xbc.runtime" subsection: the process-level resource
// knobs, resolved once during bootstrap and installed once per process.
//
// Every member is written in the form that reads best in a deployment file
// rather than the form the runtime consumes: "auto" and "75%" state an intent
// that depends on the container the process happens to land in, and turning
// them into a processor count or a byte count is the job of the resolver.
type runtimeSettings struct {
	// MaxProcs is the GOMAXPROCS value to install: "auto" derives it from this
	// container's CPU quota, a positive integer is used as written, and 0
	// leaves the process value alone.
	//
	// "auto" exists because the default is wrong in the one deployment this
	// framework is built for: inside a container Go sizes its scheduler from
	// the host's processor count, not from the quota the container is held to,
	// so a process with a 4-CPU quota on a 64-core host runs 64-wide and is
	// throttled for it.
	MaxProcs maxProcsSetting `yaml:"max_procs" default:"auto"`

	// MemoryLimit is a soft memory limit: a byte count, or a percentage of
	// this container's memory limit such as "75%". 0 leaves the process limit
	// alone.
	//
	// A soft limit does not cap the process. It tells the garbage collector
	// how much memory to trade CPU for, so a heap that approaches the
	// container limit triggers aggressive collection before the kernel kills
	// the process instead of after.
	MemoryLimit memoryLimitSetting `yaml:"memory_limit" default:"0"`

	// GCPercent is the GOGC value to install; 0 leaves the Go default of 100
	// in place. A lower value collects more often and trades throughput for
	// headroom, which is what a container whose memory limit is close to its
	// working set needs.
	GCPercent int `yaml:"gc_percent"`
}

// maxProcsSetting is xbc.runtime.max_procs as it was written.
//
// It is a defined string kind implementing encoding.TextUnmarshaler because
// that is the shape the configuration layer binds cleanly: an environment
// variable and a default tag both reach UnmarshalText, and a YAML string reaches
// it through the binder's text-unmarshaller hook. The parsing itself
// deliberately does not happen in UnmarshalText -- the binder also converts an
// unquoted YAML integer to this text without calling it -- so that one function
// produces every diagnostic for this key, whatever the source was.
type maxProcsSetting string

func (v *maxProcsSetting) UnmarshalText(text []byte) error {
	*v = maxProcsSetting(strings.TrimSpace(string(text)))
	return nil
}

// maxProcsSpec is the parsed form of xbc.runtime.max_procs.
type maxProcsSpec struct {
	// auto asks for the count to be derived from the container's CPU quota.
	auto bool
	// count is the configured processor count, where 0 is the documented off
	// switch rather than another way of writing "auto".
	count int
}

func (v maxProcsSetting) parse() (maxProcsSpec, error) {
	text := strings.TrimSpace(string(v))
	if text == "" {
		// A blank value is refused rather than read as "unset", because this
		// knob's whole purpose is to bound a process's parallelism: a blank
		// substitution in a manifest would otherwise silently restore the
		// unbounded default. The message says so, because the generic remedy
		// ("use 0") is already what a blank value looks like it means.
		return maxProcsSpec{}, fmt.Errorf(
			"xbc: %s.runtime.max_procs is empty; write \"auto\", a positive processor count, or 0 to leave GOMAXPROCS alone",
			settingsSection)
	}
	if strings.EqualFold(text, "auto") {
		return maxProcsSpec{auto: true}, nil
	}
	count, err := strconv.Atoi(text)
	if err != nil {
		return maxProcsSpec{}, fmt.Errorf(
			"xbc: %s.runtime.max_procs must be \"auto\", a positive processor count, or 0, got %q; use 0 to leave GOMAXPROCS alone",
			settingsSection, text)
	}
	if count < 0 {
		return maxProcsSpec{}, fmt.Errorf(
			"xbc: %s.runtime.max_procs must be \"auto\", a positive processor count, or 0, got %q; a negative count is not an off switch, write 0 for that",
			settingsSection, text)
	}
	return maxProcsSpec{count: count}, nil
}

// memoryLimitSetting is xbc.runtime.memory_limit as it was written. It follows
// maxProcsSetting's shape and its reasoning for parsing outside UnmarshalText.
type memoryLimitSetting string

func (v *memoryLimitSetting) UnmarshalText(text []byte) error {
	*v = memoryLimitSetting(strings.TrimSpace(string(text)))
	return nil
}

// memoryLimitSpec is the parsed form of xbc.runtime.memory_limit. Exactly one
// of the two members is set: percent when the value was written as a percentage
// of the container limit, bytes when it was written as an absolute size, and
// neither when the limit is left alone.
type memoryLimitSpec struct {
	bytes   int64
	percent float64
}

func (v memoryLimitSetting) parse() (memoryLimitSpec, error) {
	text := strings.TrimSpace(string(v))
	// Blank is refused for the same reason maxProcsSetting refuses it: this knob
	// exists to bound what the process may allocate, and silently reading a blank
	// substitution as "no limit" would remove the protection the key was added
	// for. The message names the empty case instead of offering the generic
	// remedy, which would be misleading here.
	if text == "" {
		return memoryLimitSpec{}, fmt.Errorf(
			"xbc: %s.runtime.memory_limit is empty; write a byte count, a percentage such as \"75%%\", or 0 to leave the limit alone",
			settingsSection)
	}
	if strings.HasSuffix(text, "%") {
		percent, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(text, "%")), 64)
		if err != nil || percent <= 0 || percent > 100 {
			return memoryLimitSpec{}, fmt.Errorf(
				"xbc: %s.runtime.memory_limit percentage must be greater than 0 and at most 100, got %q; the percentage is taken of this container's memory limit",
				settingsSection, text)
		}
		return memoryLimitSpec{percent: percent}, nil
	}
	bytes, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return memoryLimitSpec{}, fmt.Errorf(
			"xbc: %s.runtime.memory_limit must be a byte count, a percentage such as \"75%%\", or 0, got %q; write the size in bytes, a human-readable size is not parsed",
			settingsSection, text)
	}
	if bytes < 0 {
		return memoryLimitSpec{}, fmt.Errorf(
			"xbc: %s.runtime.memory_limit must be a byte count, a percentage such as \"75%%\", or 0, got %q; a negative limit is not an off switch, write 0 for that",
			settingsSection, text)
	}
	return memoryLimitSpec{bytes: bytes}, nil
}

// parseRuntimeSection parses and validates the whole xbc.runtime subsection.
//
// loadSettings calls it while binding, so a mistyped knob fails at startup
// beside every other configuration error, and resolveRuntimeKnobs calls it
// again while resolving, so the value that is installed is parsed by the same
// function that accepted it rather than by a second reading of the same text.
func parseRuntimeSection(section runtimeSettings) (maxProcsSpec, memoryLimitSpec, error) {
	maxProcs, err := section.MaxProcs.parse()
	if err != nil {
		return maxProcsSpec{}, memoryLimitSpec{}, err
	}
	memoryLimit, err := section.MemoryLimit.parse()
	if err != nil {
		return maxProcsSpec{}, memoryLimitSpec{}, err
	}
	if section.GCPercent < 0 {
		return maxProcsSpec{}, memoryLimitSpec{}, fmt.Errorf(
			"xbc: %s.runtime.gc_percent must not be negative, got %d; 0 leaves the Go default of 100 in place",
			settingsSection, section.GCPercent)
	}
	return maxProcs, memoryLimit, nil
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
	// pre_stop_timeout has the asymmetry slow_startup_after has and
	// shutdown_timeout does not: 0 is a documented off switch rather than a
	// mistake, because the phase is optional -- nothing has to be retracted --
	// while a negative duration is a value no author can have meant. Reading
	// it as "off" would be the silent misreading this rejects: a deployment
	// that wrote -1s to disable the phase would get a phase it could not
	// disable, and one that wrote it by accident would get no phase at all.
	if s.PreStopTimeout < 0 {
		return s, fmt.Errorf(
			"xbc: %s.pre_stop_timeout must not be negative, got %s; 0s skips the pre-stop phase, which is how it is turned off",
			settingsSection, s.PreStopTimeout)
	}
	// The process-level knobs are validated here rather than where they are
	// installed, so an unusable xbc.runtime section is reported beside every
	// other configuration mistake and before any plugin is constructed.
	if _, _, err := parseRuntimeSection(s.Runtime); err != nil {
		return s, err
	}
	// The process identity is settled here, once, for the same reason: a
	// placement source records it as the holder of what it claims, and a
	// value that differed between the acquisition that recorded it and the
	// renewal that presents it would break the very lease it names.
	s.instanceID = strings.TrimSpace(s.InstanceID)
	if s.instanceID == "" {
		derived, err := deriveInstanceID(time.Now())
		if err != nil {
			return s, err
		}
		s.instanceID = derived
	}
	if strings.ContainsAny(s.instanceID, " \t\r\n") {
		return s, fmt.Errorf(
			"xbc: %s.instance_id must not contain whitespace, got %q; it is presented verbatim as this process's identity",
			settingsSection, s.instanceID)
	}
	return s, nil
}
