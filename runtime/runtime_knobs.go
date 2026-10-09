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
	// knobSourceRuntime is a value the Go runtime computes for itself. It is
	// what xbc.runtime.max_procs "auto" resolves to: the runtime reads the
	// container's CPU quota wherever the container's cgroup is mounted and
	// keeps re-reading it while the process lives, neither of which this
	// package can do on its behalf.
	knobSourceRuntime knobSource = "runtime"
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
//	max_procs=auto (runtime) memory_limit=1.5GiB (cgroup, 75%) gc_percent=default (unset)
//
// A knob left alone reads "default", because the number that ends up in force
// is then whatever Go computed for this process -- the host's processor count
// for GOMAXPROCS, 100 for the GC percentage -- and neither value was chosen by
// this configuration. max_procs reads "auto" rather than "default" when the
// configuration asked the runtime to decide, so the line distinguishes that
// deliberate choice from a knob nobody wrote.
func (k runtimeKnobs) describe() string {
	return fmt.Sprintf("max_procs=%s (%s) memory_limit=%s (%s) gc_percent=%s (%s)",
		k.maxProcsKnob(), k.maxProcsSource,
		bytesKnob(k.memoryLimit), k.memoryLimitSource,
		countKnob(k.gcPercent), k.gcPercentSource)
}

// maxProcsKnob renders the processor count the way the line shows it: "auto"
// when the runtime is deciding for itself, the installed count otherwise.
func (k runtimeKnobs) maxProcsKnob() string {
	if k.maxProcsSource == knobSourceRuntime {
		return "auto"
	}
	return countKnob(k.maxProcs)
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
		// Nothing is installed, and that is the whole behaviour: the Go
		// runtime sizes the scheduler from the container's CPU quota itself,
		// looking wherever that container's cgroup is actually mounted (a
		// systemd slice is not the root this package would read), and it keeps
		// re-reading the quota while the process lives. Installing a count
		// here would be narrower and strictly worse -- GOMAXPROCS(n) turns
		// that periodic re-check off -- so "auto" means the same thing a
		// positive integer cannot: leave the decision with the component that
		// can keep making it.
		knobs.maxProcsSource = knobSourceRuntime
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

// containerMemoryLimit returns the memory this container's cgroup allows.
//
// The unified hierarchy states it in memory.max, where the literal "max" means
// unlimited. The legacy hierarchy keeps it in memory/memory.limit_in_bytes and
// has no word for unlimited at all -- see unlimitedMemoryLimit. An absent,
// malformed or non-positive file is underivable rather than zero, because
// installing a zero limit would put the process into a permanent GC death
// spiral instead of leaving it unlimited.
//
// The two layouts are tried in that order and the first one present is
// authoritative: memory.max decides when it is present, so its "max" is read as
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
