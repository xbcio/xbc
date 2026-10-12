package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/plugin"
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
	// It is the second of a stop's two outer budgets. PreStopTimeout runs
	// first and is accounted separately, so the worst case for one stop is
	// PreStopTimeout + ShutdownTimeout (2s + 25s by default). The default sum
	// is deliberately kept below Kubernetes' default
	// terminationGracePeriodSeconds of 30s, so a process that keeps the
	// framework's own defaults is not killed mid-shutdown by a supervisor
	// using its own default grace period; DrainTimeout runs inside this
	// budget, not beside it (see DrainTimeout), so it does not add to that
	// sum. Keeping PreStopTimeout and ShutdownTimeout apart is what lets a
	// deployment that needs a long lease-release window widen that window
	// without handing every Stop a budget it does not need.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" default:"25s"`

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

	// DrainTimeout is the budget for the drain phase: every remaining
	// (non-ingress) Drainer's Drain hook runs inside this one phase budget,
	// sequentially in reverse start order, for the same reason PreStopTimeout
	// and ShutdownTimeout are whole-phase rather than per-plugin. The budget
	// is measured from the moment the phase begins -- after the ingress
	// closure has stopped -- not from the start of the shutdown walk, so a
	// slow ingress Stop is bounded by ShutdownTimeout alone and cannot spend
	// a budget meant for the drainers that follow it.
	//
	// Unlike PreStopTimeout, it is contained in ShutdownTimeout rather than
	// accounted beside it: the drain phase runs between the ingress stop and
	// the remaining Stops, all three inside the one shutdown budget a
	// supervisor's kill timeout has to allow for, so DrainTimeout does not
	// enlarge that ceiling the way PreStopTimeout does. loadSettings therefore
	// rejects a DrainTimeout that is not strictly less than ShutdownTimeout
	// when DrainTimeout is positive -- a drain phase that could consume the
	// whole shutdown budget, or more of it than there is, would leave no
	// budget at all for the Stops that still have to run afterward.
	//
	// There deliberately is no `default` tag on this field. A fixed default
	// would have to satisfy the containment check against whatever
	// ShutdownTimeout a deployment chooses, and no single fixed value does
	// that for every ShutdownTimeout a deployment may pick -- the three
	// bundled examples that lower ShutdownTimeout to 15s are exactly the case
	// a fixed 15s default cannot survive. loadSettings instead derives the
	// unset default from the effective ShutdownTimeout, as 60% of it
	// (ShutdownTimeout*3/5): 25s -> 15s, 15s -> 9s, 5s -> 3s. A derived value
	// is by construction always strictly less than ShutdownTimeout, so it can
	// never trip the containment check below. "Unset" has to be distinguished
	// from "explicitly set", including to 0s, for the derivation to apply only
	// when nobody asked for anything else; loadSettings does so with
	// Environment.Exists on this field's path, read before Bind runs on this
	// section -- Bind's own syncBack would otherwise make every leaf, defaulted
	// or not, read back as "exists" once bound (see Environment.Exists).
	//
	// Explicitly set to 0s, it skips the phase entirely: no Drain hook is
	// called. That is the documented off switch, matching PreStopTimeout's.
	// Explicitly set to a negative value, it is not an off switch, it is a
	// mistake, and loadSettings rejects it. Explicitly set to a value greater
	// than or equal to the effective ShutdownTimeout, it is rejected by the
	// same containment check the derived default is built to always satisfy.
	DrainTimeout time.Duration `yaml:"drain_timeout"`

	// StopReserve is the tail of ShutdownTimeout held back for the Stops that
	// run after the drain phase. The ingress stop and the drain phase both run
	// under ShutdownTimeout less this reserve; the remaining Stops keep the
	// whole budget. Those remaining Stops are the ones furthest down the
	// dependency graph -- a tracing exporter's flush, a producer, an outbox --
	// and the ingress in front of them is the plugin most likely to spend the
	// entire budget while honouring its context: an HTTP server waits for its
	// slowest request until the deadline it is handed. Without a reserve that
	// wait leaves every flush behind it not attempted.
	//
	// Like DrainTimeout it carries no `default` tag and is contained in
	// ShutdownTimeout rather than added to it, so it does not change what a
	// supervisor has to allow for. Left unset it is derived as the smaller of
	// 20% of ShutdownTimeout and half of what the drain phase leaves
	// (ShutdownTimeout - DrainTimeout): 25s with the derived 15s drain ->
	// 5s. The second term keeps a derived reserve beside an explicit, long
	// drain_timeout from crowding the drain out. Explicitly set to 0s it
	// gives every phase the whole budget, which is the off switch; negative
	// is rejected, and a positive value must leave the drain phase room:
	// DrainTimeout + StopReserve must be less than ShutdownTimeout.
	StopReserve time.Duration `yaml:"stop_reserve"`

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

	// Service names the service this process is one replica of. It feeds the
	// process-wide identity every consumer stamps -- log fields, tracing
	// resource, build-info metric, startup report -- so one process never
	// presents two spellings of itself. Both members are optional and each
	// falls back to what the build itself says; see ServiceIdentity.
	Service serviceSettings `yaml:"service"`

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

	// serviceName and serviceVersion are Service.Name/Service.Version with the
	// derived defaults applied, resolved once while binding for the same
	// reason instanceID is: they are stamped onto every log line and every
	// exported span, and a value recomputed per read could disagree with
	// itself between two consumers of the same process.
	serviceName    string
	serviceVersion string
}

// serviceSettings is the "xbc.service" subsection. Both members are optional:
// an unset name falls back to the built module's last path segment and then to
// the executable's file name, and an unset version falls back to the build's
// module version and then to its VCS revision -- so the common deployment that
// builds from source gets a truthful identity without writing any of this.
type serviceSettings struct {
	// Name is the service name, shared by every replica of the deployment.
	//
	// Set it when the built artifact's name is an accident of packaging --
	// renamed executables, containers built from a shared base -- because this
	// string is what an operator sees in every log line and every trace. It
	// must not contain whitespace; the value is presented verbatim.
	Name string `yaml:"name"`

	// Version is the service build, shared by every replica of the
	// deployment. Set it when releases are cut outside the Go toolchain --
	// a CI pipeline stamping its own version -- because the derived value
	// only knows what the module and its VCS metadata say.
	Version string `yaml:"version"`
}

// Instance returns this process's stable identity: the configured instance_id
// when one was given, and the derived default otherwise.
func (s settings) Instance() string { return s.instanceID }

// ServiceIdentity returns the process-wide identity every consumer reports:
// the service name and version this deployment runs, and the instance this
// process carries. The values are settled once while binding, so every call
// answers identically.
func (s settings) ServiceIdentity() plugin.ServiceIdentity {
	return plugin.ServiceIdentity{Name: s.serviceName, Version: s.serviceVersion, Instance: s.instanceID}
}

// IngressStopBudget returns the window the ingress stop (phase A of the
// shutdown walk) runs under: ShutdownTimeout less the reserve StopReserve
// holds back for the Stops after the drain phase. It is the number the
// runtime derives phase A's deadline from in shutdown.go -- both read these
// one bound settings value, so they cannot disagree -- and what a transport
// compares its pre_drain_delay against at assembly, because a pre-drain
// window at or past this budget leaves the graceful stop no time at all.
//
// Zero means "unknown": settings that were never bound carry no budgets, and
// a caller must read 0 as "cannot judge" rather than as "no time". Bound
// settings can never produce 0 here -- loadSettings rejects a shutdown_timeout
// without a positive value and a reserve that is not strictly smaller than
// it -- which is exactly why 0 is free to carry the unknown meaning.
func (s settings) IngressStopBudget() time.Duration {
	if s.ShutdownTimeout <= 0 {
		return 0
	}
	return s.ShutdownTimeout - s.StopReserve
}

// readBuildInfo is the injectable seam over the Go toolchain's build
// metadata. It is a variable so tests can hand the fallback chain a
// documented shape -- a (devel) version, no VCS settings, a dirty tree --
// instead of whatever this repository's own build happens to carry; nothing
// outside this package may reassign it. A build stripped of its metadata
// answers nil, which every caller reads as "nothing recorded".
var readBuildInfo = func() *debug.BuildInfo {
	info, _ := debug.ReadBuildInfo()
	return info
}

// deriveServiceName composes the default service name: the last path segment
// of the main module path -- "github.com/xbcio/xbc/examples/quickstart" reads
// as "quickstart" -- and the executable's file name when the build carries no
// module path. "unknown" when neither answers, so the identity is never the
// empty string.
func deriveServiceName() string {
	if info := readBuildInfo(); info != nil && info.Main.Path != "" {
		if base := path.Base(info.Main.Path); base != "" && base != "." && base != "/" {
			return base
		}
	}
	if name := executableName(); name != "" && name != "." {
		return name
	}
	return "unknown"
}

// deriveServiceVersion composes the default service version from what the
// build records, in falling order: the main module's version, unless it is
// the toolchain's "(devel)" placeholder; the VCS revision, shortened to its
// conventional 12 characters and marked "-dirty" when the tree had local
// modifications at build time; and "unknown" when the build carries neither.
func deriveServiceVersion() string {
	info := readBuildInfo()
	if info == nil {
		return "unknown"
	}
	if version := info.Main.Version; version != "" && version != "(devel)" {
		return version
	}
	if revision := buildSetting(info, "vcs.revision"); revision != "" {
		short := revision
		if len(short) > 12 {
			short = short[:12]
		}
		if buildSetting(info, "vcs.modified") == "true" {
			short += "-dirty"
		}
		return short
	}
	return "unknown"
}

// buildSetting reads one key of the build's settings list: the key/value pairs
// the toolchain records (VCS revision, modified flag, build flags). An absent
// key answers the empty string, which every caller reads as "not recorded".
func buildSetting(info *debug.BuildInfo, key string) string {
	for _, setting := range info.Settings {
		if setting.Key == key {
			return setting.Value
		}
	}
	return ""
}

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
// that depends on the container the process happens to land in, and deciding
// what to install for each of them is the job of the resolver.
type runtimeSettings struct {
	// MaxProcs is the GOMAXPROCS value to install: "auto" leaves the count to
	// the Go runtime, a positive integer is used as written, and 0 also leaves
	// the process value alone.
	//
	// "auto" is the default because it is the one setting that stays right as
	// a container's quota changes: the Go runtime sizes the scheduler from the
	// container's CPU quota, reading it wherever that container's cgroup is
	// mounted, and re-reads it as the process runs. Setting a count -- here or
	// through the GOMAXPROCS environment variable -- turns that re-reading off,
	// so a deployment writes one only when it wants a fixed width that
	// deliberately ignores the quota.
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

	// GCPercent is the GOGC value to install; 0 installs nothing, which leaves
	// the process's own value in place -- the Go default of 100, or whatever
	// GOGC said at startup, GOGC=off included. A lower value collects more
	// often and trades throughput for headroom, which is what a container whose
	// memory limit is close to its working set needs.
	//
	// Disabling collection has no spelling of its own here on purpose: "off"
	// is the runtime's word for it, and a second name for one setting is a
	// second thing to keep in sync. Negative values are refused rather than
	// read as that word, because -1 is exactly the argument
	// debug.SetGCPercent treats as "off": a mistyped sign should not turn
	// "collect more often" into "never collect".
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
	// auto asks the Go runtime to size GOMAXPROCS for the container, which is
	// what it does when nothing installs a count.
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
			"xbc: %s.runtime.max_procs is empty; write \"auto\" to let the Go runtime size it, a positive processor count, or 0 to leave the process value alone",
			settingsSection)
	}
	if strings.EqualFold(text, "auto") {
		return maxProcsSpec{auto: true}, nil
	}
	count, err := strconv.Atoi(text)
	if err != nil {
		return maxProcsSpec{}, fmt.Errorf(
			"xbc: %s.runtime.max_procs must be \"auto\", a positive processor count, or 0, got %q; write 0 to leave the process value alone",
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
			"xbc: %s.runtime.gc_percent must not be negative, got %d; 0 installs nothing, which leaves the process's own value (the Go default of 100, or GOGC, including GOGC=off) in place",
			settingsSection, section.GCPercent)
	}
	return maxProcs, memoryLimit, nil
}

// drainTimeoutPath is the dotted configuration path checked against
// Environment.Exists before Bind runs on settingsSection, which is what lets
// loadSettings tell "nobody set this" apart from "set to the zero value".
// Checking it before Bind matters: Bind's syncBack writes every bound leaf
// back into the Environment's tree, defaulted or not, so the same check made
// after Bind would read true unconditionally (see Environment.Exists).
const drainTimeoutPath = settingsSection + ".drain_timeout"

// stopReservePath is checked against Environment.Exists before Bind for the
// same reason drainTimeoutPath is.
const stopReservePath = settingsSection + ".stop_reserve"

// deriveStopReserve computes the unset default for xbc.stop_reserve: the
// smaller of 20% of shutdown and half of what the drain phase leaves. Given a
// drain strictly less than shutdown, which loadSettings has already ensured,
// the result always leaves the drain phase a positive share.
func deriveStopReserve(shutdown, drain time.Duration) time.Duration {
	return min(shutdown/5, (shutdown-drain)/2)
}

// deriveDrainTimeout computes the unset default for xbc.drain_timeout: 60% of
// the effective shutdown_timeout (shutdown*3/5). A derived value is by
// construction always strictly less than shutdown, so it can never trip the
// containment check loadSettings applies to an explicit value, whatever
// shutdown_timeout a deployment chooses -- which is exactly why this is
// derived rather than carried as a fixed `default` tag (see DrainTimeout).
func deriveDrainTimeout(shutdown time.Duration) time.Duration {
	return shutdown * 3 / 5
}

// loadSettings binds the "xbc" section onto a zero settings and validates it.
//
// Environment.Bind applies the whole file/ENV/default chain and syncs the
// result back into the underlying koanf tree, so settings' `default` tags are
// visible to Config().Get("xbc.shutdown_timeout") afterwards, not just on the
// struct field. drain_timeout carries no `default` tag; loadSettings derives
// its unset value itself, after shutdown_timeout is known, which is why that
// one field is resolved below rather than through the tag chain Bind applies
// to everything else.
func loadSettings(env *config.Environment) (settings, error) {
	// Read before Bind: see drainTimeoutPath.
	drainTimeoutSet := env.Exists(drainTimeoutPath)
	stopReserveSet := env.Exists(stopReservePath)
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
	if !drainTimeoutSet {
		// Unset: derive the default from the now-known effective
		// shutdown_timeout rather than reading a fixed tag. The derived value
		// is by construction always strictly less than ShutdownTimeout (see
		// deriveDrainTimeout), so it is never rejected and the containment
		// check below never has to run against it.
		s.DrainTimeout = deriveDrainTimeout(s.ShutdownTimeout)
	} else {
		// drain_timeout has the identical off-switch asymmetry pre_stop_timeout
		// has, for the identical reason: the drain phase is optional, and a
		// negative value can only be a mistake. This only applies to an
		// explicit value -- the derived default is never negative.
		if s.DrainTimeout < 0 {
			return s, fmt.Errorf(
				"xbc: %s.drain_timeout must not be negative, got %s; 0s skips the drain phase, which is how it is turned off",
				settingsSection, s.DrainTimeout)
		}
		// drain_timeout is contained in shutdown_timeout -- the drain phase
		// runs between the ingress stop and the remaining Stops, all three
		// inside the one shutdown budget -- so a drain_timeout that is not
		// strictly smaller would leave no budget at all for the Stops that
		// still have to run once the drain phase returns. The check is
		// skipped when drain_timeout is 0: the off switch always fits,
		// however small shutdown_timeout is. This only applies to an explicit
		// value -- the derived default always satisfies it.
		if s.DrainTimeout > 0 && s.DrainTimeout >= s.ShutdownTimeout {
			return s, fmt.Errorf(
				"xbc: %s.drain_timeout (%s) must be less than %s.shutdown_timeout (%s); the drain phase runs inside the shutdown budget, so lower %s.drain_timeout or raise %s.shutdown_timeout",
				settingsSection, s.DrainTimeout, settingsSection, s.ShutdownTimeout, settingsSection, settingsSection)
		}
	}
	if !stopReserveSet {
		s.StopReserve = deriveStopReserve(s.ShutdownTimeout, s.DrainTimeout)
	} else {
		if s.StopReserve < 0 {
			return s, fmt.Errorf(
				"xbc: %s.stop_reserve must not be negative, got %s; 0s gives every shutdown phase the whole budget, which is how it is turned off",
				settingsSection, s.StopReserve)
		}
		// The reserve and the drain phase are both carved out of the one
		// shutdown budget, so together they must leave the ingress stop some
		// of it. 0s always fits.
		if s.StopReserve > 0 && s.DrainTimeout+s.StopReserve >= s.ShutdownTimeout {
			return s, fmt.Errorf(
				"xbc: %s.stop_reserve (%s) plus %s.drain_timeout (%s) must be less than %s.shutdown_timeout (%s); both are held inside the shutdown budget, so lower one of them or raise %s.shutdown_timeout",
				settingsSection, s.StopReserve, settingsSection, s.DrainTimeout, settingsSection, s.ShutdownTimeout, settingsSection)
		}
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
	// The service identity's two halves are settled beside it, once, for a
	// related reason: they are stamped onto every JSON log line and every
	// exported span, and a value recomputed per consumer could disagree with
	// itself within one process. An unset name and version each fall back to
	// what the build records, so a deployment that never writes this section
	// still gets a truthful identity instead of an empty one.
	s.serviceName = strings.TrimSpace(s.Service.Name)
	if s.serviceName == "" {
		s.serviceName = deriveServiceName()
	}
	if strings.ContainsAny(s.serviceName, " \t\r\n") {
		return s, fmt.Errorf(
			"xbc: %s.service.name must not contain whitespace, got %q; it is presented verbatim as this service's name",
			settingsSection, s.serviceName)
	}
	s.serviceVersion = strings.TrimSpace(s.Service.Version)
	if s.serviceVersion == "" {
		s.serviceVersion = deriveServiceVersion()
	}
	if strings.ContainsAny(s.serviceVersion, " \t\r\n") {
		return s, fmt.Errorf(
			"xbc: %s.service.version must not contain whitespace, got %q; it is presented verbatim as this service's version",
			settingsSection, s.serviceVersion)
	}
	return s, nil
}
