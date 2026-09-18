package runtime

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	goruntime "runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

// defaultCgroupRoot is where a container's control groups are mounted, for both
// the unified (v2) and the legacy (v1) hierarchy. The two layouts put the files
// this package reads at different places below it, and both are read from here
// because the framework cannot know which one its container runs.
const defaultCgroupRoot = "/sys/fs/cgroup"

// cgroupRoot is the hierarchy the container-limit derivation reads. It is a
// variable rather than the constant above so tests can point the derivation at
// a synthetic hierarchy built in a temporary directory; production code never
// assigns it.
var cgroupRoot = defaultCgroupRoot

// unlimitedMemoryLimit is the smallest byte count read as "this container has
// no memory limit". The kernel does not spell that case with a word in cgroup
// v1: it reports a page-aligned value just below MaxInt64 (9223372036854771712
// on a 4KiB-page host), so a limit is only believed when it is below this.
// 4EiB is unreachable as a real limit and far above any value a page-aligned
// subtraction could produce.
const unlimitedMemoryLimit = 1 << 62

// maxDerivedCPUs bounds what a cpu.max file may claim. A quota above this is a
// malformed control file rather than a container with a million processors, and
// installing it would be worse than installing nothing.
const maxDerivedCPUs = 1 << 20

// knobSource names where an effective process-level value came from. These
// words are the vocabulary of the startup line, so an operator reading one boot
// can tell a value derived from the container's own limits from one somebody
// typed into the configuration file.
type knobSource string

const (
	// knobSourceCgroup is a value derived from this container's cgroup limits.
	knobSourceCgroup knobSource = "cgroup"
	// knobSourceExplicit is a value configured under xbc.runtime.
	knobSourceExplicit knobSource = "explicit"
	// knobSourceUnset is a knob nobody configured and nothing was derivable for.
	knobSourceUnset knobSource = "unset"
)

// runtimeKnobs is the resolved process-level resource configuration: what the
// framework decided to install, and for each knob where that decision came
// from. A zero value means "install nothing for this knob", which is why the
// source is carried separately rather than being inferred from the value.
//
// The three knobs are process-global and therefore framework-owned: a plugin
// that sets GOMAXPROCS, GOGC or the memory limit is tuning the whole process
// from inside one component, and this type is the single place the runtime
// decides otherwise. tests/architecture holds the guard that keeps it that way.
type runtimeKnobs struct {
	// maxProcs is the processor count to install; <= 0 leaves GOMAXPROCS alone.
	maxProcs       int
	maxProcsSource knobSource

	// memoryLimit is the byte limit to install; <= 0 leaves it alone.
	memoryLimit       int64
	memoryLimitSource knobSource

	// gcPercent is the GC percentage to install; <= 0 leaves it alone.
	gcPercent       int
	gcPercentSource knobSource
}

// describe renders the effective values as the single line the runtime logs
// once per boot and the doctor command reports, for example:
//
//	max_procs=4 (cgroup) memory_limit=1.5GiB (cgroup, 75%) gc_percent=default (unset)
//
// A knob left alone reads "default", because the number that ends up in force
// is then whatever Go computed for this process -- the host's processor count
// for GOMAXPROCS, 100 for the GC percentage -- and neither value was chosen by
// this configuration.
func (k runtimeKnobs) describe() string {
	return fmt.Sprintf("max_procs=%s (%s) memory_limit=%s (%s) gc_percent=%s (%s)",
		countKnob(k.maxProcs), k.maxProcsSource,
		bytesKnob(k.memoryLimit), k.memoryLimitSource,
		countKnob(k.gcPercent), k.gcPercentSource)
}

// resolveRuntimeKnobs turns the xbc.runtime section into the values to install.
//
// It is pure: nothing process-global changes until applyRuntimeKnobs runs. That
// separation is what lets the read-only doctor command resolve, validate and
// print the very same values without mutating the process it is diagnosing.
//
// root is the cgroup hierarchy to read; resolveRuntimeKnobs never falls back to
// a default, so a caller that wants the real one says so explicitly.
func resolveRuntimeKnobs(section runtimeSettings, root string) (runtimeKnobs, error) {
	maxProcs, memoryLimit, err := parseRuntimeSection(section)
	if err != nil {
		return runtimeKnobs{}, err
	}

	knobs := runtimeKnobs{
		maxProcsSource:    knobSourceUnset,
		memoryLimitSource: knobSourceUnset,
		gcPercentSource:   knobSourceUnset,
	}

	switch {
	case maxProcs.auto:
		// Deriving nothing is not an error here: a bare-metal host has no
		// quota to derive from, and "auto" then means the Go default that is
		// already in force. The line still says so, because an operator who
		// asked for a container-derived value needs to know they did not get
		// one.
		if count, ok := containerCPUs(root); ok {
			knobs.maxProcs = count
			knobs.maxProcsSource = knobSourceCgroup
		} else {
			knobs.maxProcsSource = knobSourceUnset + ", no cgroup cpu quota"
		}
	case maxProcs.count > 0:
		knobs.maxProcs = maxProcs.count
		knobs.maxProcsSource = knobSourceExplicit
	}

	switch {
	case memoryLimit.percent > 0:
		// A percentage with nothing to take a percentage of is a configuration
		// mistake, not a knob to skip: the deployment asked for a bound
		// relative to the container and there is no container limit, so
		// silently leaving the limit alone would remove the protection the
		// percentage was written for.
		limit, ok := containerMemoryLimit(root)
		if !ok {
			return runtimeKnobs{}, fmt.Errorf(
				"xbc: %s.runtime.memory_limit is a percentage but this container has no derivable memory limit under %s; write an absolute byte count instead",
				settingsSection, root)
		}
		bytes := int64(float64(limit) * memoryLimit.percent / 100)
		if bytes < 1 {
			bytes = 1
		}
		knobs.memoryLimit = bytes
		knobs.memoryLimitSource = knobSource(fmt.Sprintf("%s, %g%%", knobSourceCgroup, memoryLimit.percent))
	case memoryLimit.bytes > 0:
		knobs.memoryLimit = memoryLimit.bytes
		knobs.memoryLimitSource = knobSourceExplicit
	}

	if section.GCPercent > 0 {
		knobs.gcPercent = section.GCPercent
		knobs.gcPercentSource = knobSourceExplicit
	}

	return knobs, nil
}

// applyRuntimeKnobs installs the resolved knobs into the process. It runs once,
// from bootstrap, and only for commands that actually start an application: a
// read-only diagnostic resolves the same values and prints them instead.
//
// The three are installed in the order they are described in, which is also the
// order the effective-value line reads. That order is not load-bearing: each
// setter recomputes the runtime's own target from the values in force, so any
// order ends in the same state.
func applyRuntimeKnobs(knobs runtimeKnobs) {
	if knobs.maxProcs > 0 {
		goruntime.GOMAXPROCS(knobs.maxProcs)
	}
	if knobs.memoryLimit > 0 {
		debug.SetMemoryLimit(knobs.memoryLimit)
	}
	if knobs.gcPercent > 0 {
		debug.SetGCPercent(knobs.gcPercent)
	}
}

// containerCPUs returns the number of CPUs this container's cgroup allows.
//
// The unified hierarchy states it in cpu.max as "<quota> <period>", where a
// quota of the literal "max" means unlimited. The legacy hierarchy splits the
// same pair across cpu.cfs_quota_us and cpu.cfs_period_us and spells unlimited
// as a quota of -1. Anything missing, unreadable, malformed or non-positive is
// reported as underivable, never as zero: a zero would be installed as a
// GOMAXPROCS of one and would quietly serialize the process.
//
// The two hierarchies are tried in that order and the first one present is
// authoritative, so a unified host whose cpu.max says "max" reports underivable
// even if a legacy cpu.cfs_quota_us file also exists. That is deliberate: the
// kernel enforces one hierarchy per controller, so on such a host the v1 file is
// a leftover from another mount rather than a live limit, and reading it would
// install a quota the container is not actually held to. "No quota" is the truth
// there, and the caller treats it as leave-GOMAXPROCS-alone.
func containerCPUs(root string) (int, bool) {
	if raw, ok := readCgroupValue(filepath.Join(root, "cpu.max")); ok {
		fields := strings.Fields(raw)
		if len(fields) != 2 || fields[0] == "max" {
			return 0, false
		}
		period, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		quota, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, false
		}
		return wholeCPUs(quota, period)
	}

	quotaRaw, ok := readCgroupValue(filepath.Join(root, "cpu", "cpu.cfs_quota_us"))
	if !ok {
		return 0, false
	}
	periodRaw, ok := readCgroupValue(filepath.Join(root, "cpu", "cpu.cfs_period_us"))
	if !ok {
		return 0, false
	}
	quota, err := strconv.ParseInt(quotaRaw, 10, 64)
	if err != nil {
		return 0, false
	}
	period, err := strconv.ParseInt(periodRaw, 10, 64)
	if err != nil {
		return 0, false
	}
	return wholeCPUs(quota, period)
}

// wholeCPUs turns a quota/period pair into the processor count to install.
//
// A fractional quota is rounded up: a container granted 1.5 CPUs still owes
// this process those 1.5 CPUs, and rounding down would leave it unable to use
// what it was granted while changing nothing about the quota it is held to.
func wholeCPUs(quota, period int64) (int, bool) {
	if quota <= 0 || period <= 0 {
		return 0, false
	}
	count := math.Ceil(float64(quota) / float64(period))
	if count < 1 || count > maxDerivedCPUs {
		return 0, false
	}
	return int(count), true
}

// containerMemoryLimit returns the memory this container's cgroup allows.
//
// The unified hierarchy states it in memory.max, where the literal "max" means
// unlimited. The legacy hierarchy keeps it in memory/memory.limit_in_bytes and
// has no word for unlimited at all -- see unlimitedMemoryLimit. As with the CPU
// quota, an absent, malformed or non-positive file is underivable rather than
// zero, because installing a zero limit would put the process into a permanent
// GC death spiral instead of leaving it unlimited.
//
// The order and the single-source rule match containerCPUs, and for the same
// reason: memory.max decides when it is present, so its "max" is read as
// unlimited rather than as "look in the legacy file".
func containerMemoryLimit(root string) (int64, bool) {
	if raw, ok := readCgroupValue(filepath.Join(root, "memory.max")); ok {
		return parseMemoryLimit(raw)
	}
	raw, ok := readCgroupValue(filepath.Join(root, "memory", "memory.limit_in_bytes"))
	if !ok {
		return 0, false
	}
	return parseMemoryLimit(raw)
}

func parseMemoryLimit(raw string) (int64, bool) {
	if raw == "max" {
		return 0, false
	}
	limit, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || limit <= 0 || limit >= unlimitedMemoryLimit {
		return 0, false
	}
	return limit, true
}

// readCgroupValue reads one cgroup control file and returns its trimmed
// contents. A missing, unreadable or empty file is reported as absent rather
// than as an error, because the same code has to run in a v2 container, in a v1
// container and on a host with no cgroup mounted at all, and only the last of
// those is the caller's problem to report.
func readCgroupValue(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", false
	}
	return value, true
}

// countKnob renders a knob whose value is a count, with "default" standing for
// "the process keeps whatever Go computed for it".
func countKnob(count int) string {
	if count <= 0 {
		return "default"
	}
	return strconv.Itoa(count)
}

// bytesKnob renders a byte limit the way the effective-value line shows it:
// 1.5GiB rather than 1610612736. The limits involved are container limits,
// whose exact byte value is not a number anybody reads off a log line.
func bytesKnob(bytes int64) string {
	if bytes <= 0 {
		return "default"
	}
	return formatBytes(bytes)
}

// byteUnits are the suffixes bytesKnob scales through; the loop below never
// needs one past PiB for a limit that came from a cgroup file.
var byteUnits = []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return strconv.FormatInt(bytes, 10) + byteUnits[0]
	}
	scaled, index := float64(bytes), 0
	for scaled >= unit && index < len(byteUnits)-1 {
		scaled /= unit
		index++
	}
	return strconv.FormatFloat(math.Round(scaled*100)/100, 'f', -1, 64) + byteUnits[index]
}
