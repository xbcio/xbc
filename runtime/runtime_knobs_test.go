package runtime

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeCgroupFile writes one control file below a synthetic hierarchy rooted at
// root. Directories are created as needed, so a v1 layout and a v2 layout are
// described by the file paths the kernel would use.
func writeCgroupFile(t *testing.T, root, relative, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
}

// TestContainerCPUsDerivesFromBothCgroupLayouts covers the two hierarchies the
// same derivation has to read, and the three ways a control file can fail to
// state a quota: an explicit unlimited word, a malformed value, and no file at
// all. Every one of those must be reported as underivable rather than as zero,
// because the caller installs whatever it is handed.
func TestContainerCPUsDerivesFromBothCgroupLayouts(t *testing.T) {
	t.Parallel()
	for name, testCase := range map[string]struct {
		files   map[string]string
		want    int
		derived bool
	}{
		"v2 whole quota": {
			files:   map[string]string{"cpu.max": "200000 100000\n"},
			want:    2,
			derived: true,
		},
		"v2 fractional quota rounds up": {
			files:   map[string]string{"cpu.max": "150000 100000"},
			want:    2,
			derived: true,
		},
		"v2 unlimited": {
			files: map[string]string{"cpu.max": "max 100000"},
		},
		"v2 quota without a period": {
			files: map[string]string{"cpu.max": "200000"},
		},
		"v2 unparsable quota": {
			files: map[string]string{"cpu.max": "many 100000"},
		},
		"v2 zero period": {
			files: map[string]string{"cpu.max": "200000 0"},
		},
		"v2 empty file": {
			files: map[string]string{"cpu.max": "\n"},
		},
		"v1 quota and period": {
			files: map[string]string{
				"cpu/cpu.cfs_quota_us":  "400000",
				"cpu/cpu.cfs_period_us": "100000\n",
			},
			want:    4,
			derived: true,
		},
		"v1 no limit": {
			files: map[string]string{
				"cpu/cpu.cfs_quota_us":  "-1",
				"cpu/cpu.cfs_period_us": "100000",
			},
		},
		"v1 quota without a period file": {
			files: map[string]string{"cpu/cpu.cfs_quota_us": "400000"},
		},
		"v1 unparsable period": {
			files: map[string]string{
				"cpu/cpu.cfs_quota_us":  "400000",
				"cpu/cpu.cfs_period_us": "one hundred thousand",
			},
		},
		"no cgroup at all": {
			files: map[string]string{},
		},
		"quota below one cpu still yields one": {
			files:   map[string]string{"cpu.max": "50000 100000"},
			want:    1,
			derived: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			for relative, contents := range testCase.files {
				writeCgroupFile(t, root, relative, contents)
			}
			count, derived := containerCPUs(root)
			assert.Equal(t, testCase.derived, derived,
				"only a well-formed, bounded quota is a derivable one")
			if testCase.derived {
				assert.Equal(t, testCase.want, count)
			} else {
				assert.Zero(t, count, "an underivable quota must not be reported as a count")
			}
		})
	}
}

// TestContainerMemoryLimitReadsBothCgroupLayouts pins the two spellings of "no
// limit": cgroup v2 says "max", cgroup v1 says nothing and reports a
// page-aligned value just below MaxInt64. Both must leave the process limit
// alone, and neither may be mistaken for a real limit of 9223372036854771712
// bytes, which no container has.
func TestContainerMemoryLimitReadsBothCgroupLayouts(t *testing.T) {
	t.Parallel()
	const gib = 1 << 30
	for name, testCase := range map[string]struct {
		files   map[string]string
		want    int64
		derived bool
	}{
		"v2 limit": {
			files:   map[string]string{"memory.max": "1610612736\n"},
			want:    1536 << 20,
			derived: true,
		},
		"v2 unlimited": {
			files: map[string]string{"memory.max": "max"},
		},
		"v2 zero": {
			files: map[string]string{"memory.max": "0"},
		},
		"v2 human readable size is not parsed": {
			files: map[string]string{"memory.max": "1GiB"},
		},
		"v1 limit": {
			files:   map[string]string{"memory/memory.limit_in_bytes": "1073741824"},
			want:    gib,
			derived: true,
		},
		"v1 page-aligned sentinel": {
			files: map[string]string{"memory/memory.limit_in_bytes": "9223372036854771712"},
		},
		"v1 max int64": {
			files: map[string]string{"memory/memory.limit_in_bytes": "9223372036854775807"},
		},
		"v1 negative": {
			files: map[string]string{"memory/memory.limit_in_bytes": "-1"},
		},
		"v1 unparsable": {
			files: map[string]string{"memory/memory.limit_in_bytes": "unlimited"},
		},
		"no cgroup at all": {
			files: map[string]string{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			for relative, contents := range testCase.files {
				writeCgroupFile(t, root, relative, contents)
			}
			limit, derived := containerMemoryLimit(root)
			assert.Equal(t, testCase.derived, derived)
			if testCase.derived {
				assert.Equal(t, testCase.want, limit)
			} else {
				assert.Zero(t, limit, "an underivable limit must not be reported as a byte count")
			}
		})
	}
}

func TestResolveRuntimeKnobsUsesTheContainerQuotaOnlyForAuto(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeCgroupFile(t, root, "cpu.max", "300000 100000")
	writeCgroupFile(t, root, "memory.max", "2147483648")

	knobs, err := resolveRuntimeKnobs(runtimeSettings{
		MaxProcs:    "auto",
		MemoryLimit: "0",
	}, root)
	require.NoError(t, err)
	assert.Equal(t, 3, knobs.maxProcs, "auto must derive the count from the quota, not from the host")
	assert.Equal(t, knobSourceCgroup, knobs.maxProcsSource)
	assert.Zero(t, knobs.memoryLimit)
	assert.Equal(t, knobSourceUnset, knobs.memoryLimitSource)

	knobs, err = resolveRuntimeKnobs(runtimeSettings{MaxProcs: "8", MemoryLimit: "0"}, root)
	require.NoError(t, err)
	assert.Equal(t, 8, knobs.maxProcs, "an explicit count is used as written, cgroup or not")
	assert.Equal(t, knobSourceExplicit, knobs.maxProcsSource)

	knobs, err = resolveRuntimeKnobs(runtimeSettings{MaxProcs: "0", MemoryLimit: "0"}, root)
	require.NoError(t, err)
	assert.Zero(t, knobs.maxProcs, "0 is the documented off switch")
	assert.Equal(t, knobSourceUnset, knobs.maxProcsSource)
}

// TestResolveRuntimeKnobsLeavesGOMAXPROCSAloneWithoutAQuota is the mutation
// control for the derivation itself: were "auto" to fall back to the host's
// processor count, or to a placeholder, this case would install something
// instead of leaving the process as Go sized it.
func TestResolveRuntimeKnobsLeavesGOMAXPROCSAloneWithoutAQuota(t *testing.T) {
	t.Parallel()
	knobs, err := resolveRuntimeKnobs(runtimeSettings{MaxProcs: "auto", MemoryLimit: "0"}, t.TempDir())
	require.NoError(t, err)
	assert.Zero(t, knobs.maxProcs)
	assert.Equal(t, knobSource("unset, no cgroup cpu quota"), knobs.maxProcsSource,
		"the effective-value line has to say that nothing was derivable")
	assert.Contains(t, knobs.describe(), "max_procs=default (unset, no cgroup cpu quota)")
}

func TestResolveRuntimeKnobsResolvesAMemoryPercentageAgainstTheContainer(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeCgroupFile(t, root, "memory.max", "2147483648") // 2GiB

	knobs, err := resolveRuntimeKnobs(runtimeSettings{MaxProcs: "0", MemoryLimit: "75%"}, root)
	require.NoError(t, err)
	assert.Equal(t, int64(1610612736), knobs.memoryLimit)
	assert.Equal(t, knobSource("cgroup, 75%"), knobs.memoryLimitSource)
	assert.Equal(t, "default", countKnob(knobs.maxProcs))

	knobs, err = resolveRuntimeKnobs(runtimeSettings{MaxProcs: "0", MemoryLimit: "1610612736"}, root)
	require.NoError(t, err)
	assert.Equal(t, int64(1610612736), knobs.memoryLimit)
	assert.Equal(t, knobSourceExplicit, knobs.memoryLimitSource)

	knobs, err = resolveRuntimeKnobs(runtimeSettings{MaxProcs: "0", MemoryLimit: "0"}, root)
	require.NoError(t, err)
	assert.Zero(t, knobs.memoryLimit)
	assert.Equal(t, knobSourceUnset, knobs.memoryLimitSource)
}

// TestResolveRuntimeKnobsRejectsAPercentageWithoutAContainerLimit covers the
// one unsatisfiable combination this section allows. A percentage with nothing
// to take a percentage of is a configuration mistake rather than a knob to
// skip: skipping it would silently drop the protection the percentage was
// written for.
func TestResolveRuntimeKnobsRejectsAPercentageWithoutAContainerLimit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeCgroupFile(t, root, "memory.max", "max")

	_, err := resolveRuntimeKnobs(runtimeSettings{MaxProcs: "0", MemoryLimit: "75%"}, root)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "xbc.runtime.memory_limit", "the error must name the configuration path")
	assert.Contains(t, err.Error(), root, "the error must say where the limit was looked for")
}

func TestResolveRuntimeKnobsCarriesGCPercentOnlyWhenConfigured(t *testing.T) {
	t.Parallel()
	knobs, err := resolveRuntimeKnobs(runtimeSettings{MaxProcs: "0", MemoryLimit: "0", GCPercent: 50}, t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, 50, knobs.gcPercent)
	assert.Equal(t, knobSourceExplicit, knobs.gcPercentSource)

	knobs, err = resolveRuntimeKnobs(runtimeSettings{MaxProcs: "0", MemoryLimit: "0"}, t.TempDir())
	require.NoError(t, err)
	assert.Zero(t, knobs.gcPercent, "the Go default of 100 stays in place")
	assert.Equal(t, knobSourceUnset, knobs.gcPercentSource)
}

// TestRuntimeKnobsDescribeIsTheEffectiveValueLine pins the one line an operator
// reads to answer "what did this process actually install, and who decided".
func TestRuntimeKnobsDescribeIsTheEffectiveValueLine(t *testing.T) {
	t.Parallel()
	resolved := runtimeKnobs{
		maxProcs:          4,
		maxProcsSource:    knobSourceCgroup,
		memoryLimit:       1610612736,
		memoryLimitSource: "cgroup, 75%",
		gcPercent:         200,
		gcPercentSource:   knobSourceExplicit,
	}
	assert.Equal(t,
		"max_procs=4 (cgroup) memory_limit=1.5GiB (cgroup, 75%) gc_percent=200 (explicit)",
		resolved.describe())

	assert.Equal(t,
		"max_procs=default (unset) memory_limit=default (unset) gc_percent=default (unset)",
		runtimeKnobs{
			maxProcsSource:    knobSourceUnset,
			memoryLimitSource: knobSourceUnset,
			gcPercentSource:   knobSourceUnset,
		}.describe())
}

func TestFormatBytesKeepsTheContainerLimitsReadable(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "512B", formatBytes(512))
	assert.Equal(t, "1KiB", formatBytes(1024))
	assert.Equal(t, "1.5KiB", formatBytes(1536))
	assert.Equal(t, "1.5GiB", formatBytes(1610612736))
	assert.Equal(t, "10MiB", formatBytes(10<<20))
}

// currentGCPercent reads the process GC percentage. SetGCPercent is the only
// accessor the runtime exposes and it doubles as the setter, so the value is
// read by installing -1 -- collection off -- and putting the previous value
// straight back.
func currentGCPercent() int {
	percent := debug.SetGCPercent(-1)
	debug.SetGCPercent(percent)
	return percent
}

// TestApplyRuntimeKnobsInstallsTheResolvedValues is deliberately not parallel:
// it asserts on and restores process-global knobs, which no other test in this
// package may be running against at the same time.
func TestApplyRuntimeKnobsInstallsTheResolvedValues(t *testing.T) {
	previousProcs := goruntime.GOMAXPROCS(0)
	previousLimit := debug.SetMemoryLimit(-1)
	previousPercent := currentGCPercent()
	t.Cleanup(func() {
		goruntime.GOMAXPROCS(previousProcs)
		debug.SetMemoryLimit(previousLimit)
		debug.SetGCPercent(previousPercent)
	})

	applyRuntimeKnobs(runtimeKnobs{})

	assert.Equal(t, previousProcs, goruntime.GOMAXPROCS(0), "an unset max_procs must not touch GOMAXPROCS")
	assert.Equal(t, previousLimit, debug.SetMemoryLimit(-1), "an unset memory limit must not touch the limit")
	assert.Equal(t, previousPercent, currentGCPercent(), "an unset gc_percent must leave the Go default in place")

	applyRuntimeKnobs(runtimeKnobs{maxProcs: 3, memoryLimit: 1 << 30, gcPercent: 200})

	assert.Equal(t, 3, goruntime.GOMAXPROCS(0))
	assert.Equal(t, int64(1<<30), debug.SetMemoryLimit(-1))
	assert.Equal(t, 200, currentGCPercent())
}
