package runtime

import (
	"bytes"
	"context"
	"regexp"
	goruntime "runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// runDoctor executes the doctor subcommand against a captured writer and
// returns everything it printed.
func runDoctor(t *testing.T, app *App, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	app.out = &out
	code, err := app.Execute(context.Background(), append([]string{"doctor"}, args...))
	require.NoError(t, err)
	require.Equal(t, 0, code)
	return out.String()
}

// TestDoctorInitializesNoLoggerAndEmitsNoLogLine pins doctor's read-only
// contract at the level that matters operationally: running it against a
// production configuration must not open the log file, must not start the
// flusher goroutine, and must not replace the process-global logger, because
// an operator runs doctor precisely when they are not sure the configuration
// is safe to act on.
func TestDoctorInitializesNoLoggerAndEmitsNoLogLine(t *testing.T) {
	before := log.L()

	definition := plugin.Define("diagnosed", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	})
	app := newRuntimeTestApp(definition)
	// A configuration that would have produced console output had the logger
	// actually been installed, so that "no log line" is a real observation.
	args := []string{"--config", writeRuntimeTestConfig(t,
		"log:\n  console:\n    enabled: true\nxbc:\n  shutdown_timeout: 1s\n")}

	out := runDoctor(t, app, args...)

	assert.Equal(t, before, log.L(), "doctor must not replace the process-global logger")
	assert.Equal(t, log.Nop(), app.logger, "doctor runs against a no-op logger")
	assert.NotContains(t, out, "assembly plan complete",
		"the plan is reported by doctor's own output, never through the logger")
	assert.Contains(t, out, "xbc doctor")
	assert.Contains(t, out, "diagnosed")
}

// TestDoctorReportsGraphInstancesSourcesAndDisableReasons covers the four
// things doctor exists to answer, in one run, because a reader diagnosing "why
// is my plugin off" needs them together.
func TestDoctorReportsGraphInstancesSourcesAndDisableReasons(t *testing.T) {
	on := plugin.Define("switched-on", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Activation: plugin.WhenConfigured("plugins.switched-on")})
	off := plugin.Define("switched-off", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Activation: plugin.WhenConfigured("plugins.switched-off")})

	app := newRuntimeTestApp(on, off)
	out := runDoctor(t, app, runtimeTestConfigWith(t, time.Second,
		"plugins:\n  switched-on:\n    enabled: true\n")...)

	assert.Contains(t, out, "declared 2, enabled instances 1, disabled 1")
	assert.Contains(t, out, "plugins.switched-on", "an enabled instance names the section it binds")
	assert.Contains(t, out, "switched-off")
	assert.Contains(t, out, "activation path plugins.switched-off is not configured",
		"a disabled plugin must say why, not merely that it is off")
	assert.Contains(t, out, "sources  file ", "doctor names where configuration came from")
}

// TestDoctorPrintsNoConfiguredValue is the secret-safety guard: doctor may
// name paths, identities and source labels, but a configured value -- which in
// a real deployment is a DSN or a token -- must never reach its output.
func TestDoctorPrintsNoConfiguredValue(t *testing.T) {
	const secret = "postgres://user:hunter2@db/app"

	definition := plugin.DefineConfigured("secretive",
		plugin.ConfigSpec[secretiveConfig]{Defaults: func() secretiveConfig { return secretiveConfig{} }},
		func(plugin.BuildContext, secretiveConfig) (*runtimeTestValue, error) {
			return &runtimeTestValue{}, nil
		})

	app := newRuntimeTestApp(definition)
	out := runDoctor(t, app, runtimeTestConfigWith(t, time.Second,
		"plugins:\n  secretive:\n    dsn: \""+secret+"\"\n")...)

	assert.NotContains(t, out, secret, "doctor reports where a value came from, never the value")
	assert.NotContains(t, out, "hunter2")
	assert.Contains(t, out, "plugins.secretive", "the path itself is safe and is what the reader needs")
}

type secretiveConfig struct {
	DSN string `yaml:"dsn"`
}

// doctorContract is exported by exactly one plugin below; doctorAbsentContract
// is exported by nothing, which is the only way to produce an input that
// resolved to nothing.
type doctorContract interface{ Diagnosed() }

type doctorAbsentContract interface{ Absent() }

type doctorProducerValue struct{}

func (*doctorProducerValue) Diagnosed() {}

// TestDoctorNamesSelectionSiteAndUnsatisfiedInputs covers the two questions the
// workload grouping cannot answer: who selected this plugin, and what is
// actually feeding it.
//
// The unsatisfied optional and collecting inputs are the point. Both are legal,
// so planning succeeds, no log line is written, and nothing else in the system
// distinguishes "no exporter is enabled" from "wired correctly". If doctor stops
// printing them, an operator has no way left to discover that the extension they
// believe is attached never was.
func TestDoctorNamesSelectionSiteAndUnsatisfiedInputs(t *testing.T) {
	producer := plugin.Define("producer", func(plugin.BuildContext) (*doctorProducerValue, error) {
		return &doctorProducerValue{}, nil
	}, plugin.Options[*doctorProducerValue]{
		Exports: plugin.Contracts(plugin.ExportAs(func(value *doctorProducerValue) doctorContract { return value })),
	})
	required := plugin.RequireOne[doctorContract]()
	optional := plugin.OptionalOne[doctorAbsentContract]()
	collected := plugin.Collect[doctorAbsentContract]()
	consumer := plugin.Define("consumer", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Inputs: plugin.Inputs(required, optional, collected)})
	solitary := plugin.Define("solitary", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	})

	app := newRuntimeTestApp(producer, consumer, solitary)
	out := runDoctor(t, app, runtimeTestConfig(t, time.Second)...)

	assert.Contains(t, out, "unowned",
		"a composition declaring no workload still says which group its plugins are in")
	assert.Regexp(t, `selected at /.*runtime_test\.go:\d+`, out,
		"an instance names the composition site that selected it, as an openable source location")
	assert.Regexp(t, `requires\s+one\s+runtime\.doctorContract\s+from producer`, out,
		"a satisfied input names its producer")
	assert.Regexp(t, `requires\s+optional\s+runtime\.doctorAbsentContract\s+unsatisfied: no enabled plugin exports it`, out,
		"an optional input that resolved to nothing is legal, silent, and must still be reported")
	assert.Regexp(t, `requires\s+many\s+runtime\.doctorAbsentContract\s+unsatisfied: no enabled plugin exports it`, out,
		"a collecting input that resolved to nothing is equally invisible everywhere else")
	assert.Regexp(t, `solitary\n(?:\s+.*\n)*?\s+no declared inputs`, out,
		"a plugin with no inputs says so, so that absence is not read as a truncated report")
}

// TestEnvironmentAloneActivatesAWhenConfiguredPlugin is the end-to-end shape of
// the whole task: a container that sets one variable and mounts no
// configuration file must be able to turn a plugin on.
func TestEnvironmentAloneActivatesAWhenConfiguredPlugin(t *testing.T) {
	definition := plugin.Define("env-activated", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Activation: plugin.WhenConfigured("plugins.env-activated")})

	off := newRuntimeTestApp(definition)
	require.Contains(t, runDoctor(t, off, runtimeTestConfig(t, time.Second)...),
		"declared 1, enabled instances 0, disabled 1", "without the variable the plugin stays off")

	t.Setenv("XBC_PLUGINS_ENV_ACTIVATED_ENABLED", "true")
	on := newRuntimeTestApp(definition)
	out := runDoctor(t, on, runtimeTestConfig(t, time.Second)...)

	assert.Contains(t, out, "declared 1, enabled instances 1, disabled 0",
		"an environment variable alone must be able to activate a WhenConfigured plugin")
	assert.Contains(t, out, "from env", "doctor attributes the activation to the environment layer")
}

// TestEnvironmentAloneDeclaresMultipleInstances pins the multi-instance half of
// the same promise, including that the instance names came from nowhere but the
// environment.
func TestEnvironmentAloneDeclaresMultipleInstances(t *testing.T) {
	definition := plugin.DefineConfigured("env-multi",
		plugin.ConfigSpec[secretiveConfig]{Defaults: func() secretiveConfig { return secretiveConfig{} }},
		func(plugin.BuildContext, secretiveConfig) (*runtimeTestValue, error) {
			return &runtimeTestValue{}, nil
		},
		plugin.Options[*runtimeTestValue]{Instances: plugin.MultipleInstances})

	t.Setenv("XBC_PLUGINS_ENV_MULTI_PRIMARY_DSN", "primary")
	t.Setenv("XBC_PLUGINS_ENV_MULTI_REPLICA_DSN", "replica")

	app := newRuntimeTestApp(definition)
	out := runDoctor(t, app, runtimeTestConfig(t, time.Second)...)

	assert.Contains(t, out, "plugins.env-multi.primary")
	assert.Contains(t, out, "plugins.env-multi.replica")
	assert.Contains(t, out, "enabled instances 2",
		"instance names are discovered by environment enumeration, with no file involved")
}

// TestUnownedTopLevelKeyFailsBeforeAnythingIsConstructed pins the ownership
// rule at the level an operator meets it: a typo in a top-level section name is
// a startup failure that names the key, not a silently ignored block.
func TestUnownedTopLevelKeyFailsBeforeAnythingIsConstructed(t *testing.T) {
	definition := plugin.Define("owns-nothing-toplevel", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	})
	app := newRuntimeTestApp(definition)

	code, err := app.Execute(context.Background(),
		runtimeTestConfigWith(t, time.Second, "wbe:\n  addr: \":8080\"\n"))
	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wbe", "the error names the offending path")
	assert.Contains(t, err.Error(), "declared top-level sections",
		"the error shows what would have been accepted")
	assert.Nil(t, app.owned, "the failure happens before construction")
}

// TestDoctorConstructsNothingAndLeavesNoGoroutine guards the rest of doctor's
// read-only contract -- no connections, no goroutines, no listeners -- which
// otherwise holds only because Execute happens to return before Construct. A
// later task appends its own section to doctor; this is what stops it quietly
// dialling a database or spawning a collector and staying green.
//
// Constructing is how every one of those three appears, so a factory that
// records having run is the sharp, deterministic half of the guard. The
// goroutine delta is the blunt half, and catches anything started outside a
// factory.
//
// The composition carries a workload, so the grouping, the placement line and
// the runtime line all run under the same guard. Those sections read the plan,
// the settings and a pure resolver; a version of any of them that reached for a
// resource instead would be caught here rather than only in review.
func TestDoctorConstructsNothingAndLeavesNoGoroutine(t *testing.T) {
	var built atomic.Bool
	definition := plugin.Define("must-not-be-built", func(plugin.BuildContext) (*runtimeTestValue, error) {
		built.Store(true)
		return &runtimeTestValue{}, nil
	})
	app := doctorApp([]plugin.Bundle{
		plugin.WorkloadOf("carried", plugin.BundleOf(definition), plugin.WithReplicas(2)),
	})

	before := settledGoroutines()
	out := runDoctor(t, app, runtimeTestConfig(t, time.Second)...)

	assert.False(t, built.Load(),
		"doctor must not run a plugin factory: constructing is how connections, listeners and goroutines appear")
	assert.Nil(t, app.owned, "doctor must finish owning nothing that would need stopping")
	assert.False(t, app.trafficOpen, "doctor must not release the traffic gate")
	assert.Contains(t, out, "must-not-be-built", "the plugin is still reported; it is only not built")
	assert.Contains(t, out, "workload carried", "the grouping is part of the report the guard covers")
	assert.Contains(t, out, "placement", "the placement line is part of the report the guard covers")
	assert.Contains(t, out, "runtime", "the runtime line is part of the report the guard covers")

	assert.LessOrEqual(t, settledGoroutines(), before,
		"doctor must leave no goroutine running: it is run precisely when acting on the configuration may be unsafe")
}

// settledGoroutines samples the goroutine count once it stops moving, so that a
// goroutine still unwinding from an earlier test does not decide this guard.
func settledGoroutines() int {
	previous := goruntime.NumGoroutine()
	for attempt := 0; attempt < 50; attempt++ {
		time.Sleep(10 * time.Millisecond)
		current := goruntime.NumGoroutine()
		if current == previous {
			return current
		}
		previous = current
	}
	return previous
}

// TestApplicationFreeformRootIsAcceptedWithoutASchema pins the other side of
// that rule: app.* is declared freeform on purpose, so an application may put
// anything under it without the framework claiming to understand it.
func TestApplicationFreeformRootIsAcceptedWithoutASchema(t *testing.T) {
	definition := plugin.Define("freeform-neighbour", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	})
	app := newRuntimeTestApp(definition)

	out := runDoctor(t, app, runtimeTestConfigWith(t, time.Second,
		"app:\n  name: demo\n  anything:\n    nested: true\n")...)
	assert.Contains(t, out, "roots    app, log, plugins",
		"app must be named in the declared roots, which is precisely why the ownership walk does not reject it")
	assert.Contains(t, out, "declared 1, enabled instances 1, disabled 0",
		"a block the framework never interprets must not disturb the plugin graph")
	assert.NotContains(t, out, "demo",
		"a freeform section is still a section: doctor names it but never prints what is in it")
}

// ── placement grouping ────────────────────────────────────────────────────

// doctorApp composes an App from whole Bundles, which is what the grouping
// tests need: a composition built from Definitions alone has no workload to
// group by.
func doctorApp(bundles []plugin.Bundle) *App {
	app := newApp(bundles)
	app.ready = make(chan struct{})
	return app
}

// doctorRowPadding matches the spaces doctor pads a row's columns with.
var doctorRowPadding = regexp.MustCompile(`[ \t]+`)

// doctorRows collapses the report's column padding, so an assertion can name a
// row without also pinning it to the width of the widest row beside it: a test
// that failed because "coderanger" is longer than "sast" would be testing the
// alignment rather than the grouping.
func doctorRows(out string) string {
	return doctorRowPadding.ReplaceAllString(out, " ")
}

// doctorWorkloadBundles is the composition the grouping snapshots are read
// against: one exclusive workload, two ordinary ones, and one Definition that
// belongs to no workload. The exclusive declaration is what makes "host
// everything" a shape that has to be spelled out rather than assumed -- a
// process cannot carry sast beside the others, so the all-in case leaves it off
// by configuration.
func doctorWorkloadBundles() []plugin.Bundle {
	value := func(key plugin.Key) plugin.Definition {
		return plugin.Define(key, func(plugin.BuildContext) (*runtimeTestValue, error) {
			return &runtimeTestValue{}, nil
		})
	}
	return []plugin.Bundle{
		plugin.WorkloadOf("sast",
			plugin.BundleOf(value("sast-dispatcher"), value("sast-worker"), value("sast-api")),
			plugin.WithExclusiveProcess(), plugin.WithReplicas(3)),
		plugin.WorkloadOf("coderanger",
			plugin.BundleOf(value("coderanger-triggers"), value("coderanger-worker")),
			plugin.WithReplicas(6)),
		plugin.WorkloadOf("webscan",
			plugin.BundleOf(value("webscan-timers"), value("webscan-worker")),
			plugin.WithReplicas(6)),
		plugin.BundleOf(value("web")),
	}
}

// doctorWorkloadConfig renders the workloads block that turns the named
// workloads off, leaving the rest to the default source.
func doctorWorkloadConfig(disabled ...string) string {
	if len(disabled) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("workloads:\n")
	for _, key := range disabled {
		b.WriteString("  " + key + ":\n    enabled: false\n")
	}
	return b.String()
}

// TestDoctorGroupsTheGraphByWorkload is the report's headline answer: reading
// down the left edge tells an operator which declared workloads this process
// carries, which it does not, why not, and what each one contributed.
//
// The four shapes are the four a deployment actually produces. "Standby" is the
// one worth keeping honest: a process that carries no workload at all is not a
// misconfiguration, it is the process that owns only the unowned plugins and
// waits, so its report must read as a deliberate shape rather than as an empty
// one.
func TestDoctorGroupsTheGraphByWorkload(t *testing.T) {
	for name, testCase := range map[string]struct {
		disabled []string
		contains []string
		absent   []string
	}{
		"exclusive workload alone": {
			disabled: []string{"coderanger", "webscan"},
			contains: []string{
				"workload sast hosted exclusive replicas=3 plugins=3",
				"workload coderanger not held replicas=6 plugins=0",
				"workload webscan not held replicas=6 plugins=0",
				"sast-dispatcher",
				"sast-api",
			},
		},
		"every hostable workload": {
			disabled: []string{"sast"},
			contains: []string{
				"workload sast not held exclusive replicas=3 plugins=0",
				"workload coderanger hosted replicas=6 plugins=2",
				"workload webscan hosted replicas=6 plugins=2",
				"coderanger-triggers",
				"webscan-timers",
			},
		},
		"some hosted": {
			disabled: []string{"sast", "webscan"},
			contains: []string{
				"workload coderanger hosted replicas=6 plugins=2",
				"workload webscan not held replicas=6 plugins=0",
				"coderanger-worker",
			},
			absent: []string{"webscan-timers"},
		},
		"none hosted, the standby": {
			disabled: []string{"sast", "coderanger", "webscan"},
			contains: []string{
				"workload coderanger not held replicas=6 plugins=0",
			},
			absent: []string{"coderanger-triggers", "webscan-timers", "sast-worker"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			app := doctorApp(doctorWorkloadBundles())
			out := runDoctor(t, app, runtimeTestConfigWith(t, time.Second,
				doctorWorkloadConfig(testCase.disabled...))...)
			rows := doctorRows(out)

			for _, want := range testCase.contains {
				assert.Containsf(t, rows, want, "%s must appear in the grouped report", want)
			}
			for _, unwanted := range testCase.absent {
				assert.NotContainsf(t, rows, unwanted,
					"%s belongs to a workload this process does not carry and must not be in the graph it reports", unwanted)
			}
			assert.Contains(t, rows, "unowned plugins=1", "the unowned group is always rendered")
			assert.Contains(t, out, "roots    app, log, plugins, workloads",
				"declaring a workload adds its root, which is what makes workloads.<key> ownable at all")
		})
	}
}

// TestDoctorReportsThePlacementSourceHolderAndNotes is what makes the grouping
// readable: the rows say what was decided, and this line says who decided it
// and what they chose to explain.
//
// The notes matter most. A lease source knows which slots were already held and
// doctor cannot reconstruct that, so a workload that was declined is explained
// here or nowhere.
func TestDoctorReportsThePlacementSourceHolderAndNotes(t *testing.T) {
	source := &recordingPlacement{placement: plugin.Placement{
		Source: "lease",
		Holder: "host-7-1758091200-9f3c1a2b",
		Hosted: []plugin.WorkloadKey{"coderanger"},
		Notes: []string{
			"claimed slot 1 of 6 for workload coderanger",
			"declined sast: exclusive workload, this process already carries coderanger",
		},
	}}
	app, err := New(WithBundles(doctorWorkloadBundles()...), WithPlacement(source))
	require.NoError(t, err)
	app.ready = make(chan struct{})

	out := runDoctor(t, app, runtimeTestConfigWith(t, time.Second, doctorWorkloadConfig("sast", "webscan"))...)

	assert.Contains(t, out, "source   lease", "the source that decided is named")
	assert.Contains(t, out, "holder   host-7-1758091200-9f3c1a2b",
		"the claimant is named, so an operator can correlate the lease with the process")
	assert.Contains(t, out, "hosted   coderanger")
	assert.Contains(t, out, "claimed slot 1 of 6 for workload coderanger")
	assert.Contains(t, out, "declined sast: exclusive workload, this process already carries coderanger",
		"a source's explanation of a decline is printed verbatim; doctor cannot derive it")
	rows := doctorRows(out)
	assert.Contains(t, rows, "workload coderanger hosted replicas=6 plugins=2")
	assert.Contains(t, rows, "workload sast not held exclusive replicas=3 plugins=0",
		"an unhosted workload is listed rather than omitted")
}

// TestDoctorSpellsOutAnEmptyPlacementExplanation keeps the absent case from
// reading as a truncated report. A static decision genuinely has no notes, and
// a blank after "notes" would look like a field nobody filled in rather than
// the answer.
func TestDoctorSpellsOutAnEmptyPlacementExplanation(t *testing.T) {
	app := doctorApp(doctorWorkloadBundles())
	out := runDoctor(t, app, runtimeTestConfigWith(t, time.Second,
		doctorWorkloadConfig("sast", "webscan"))...)

	assert.Contains(t, out, "source   static")
	assert.Contains(t, out, "notes    (none)")
	assert.Contains(t, out, "holder   (none; only a source that records a claimant names one)",
		"a static decision names no claimant, and saying so beats an empty field")
}

// TestDoctorReportsTheRuntimeKnobsFromTheContainer is the second half of "what
// does this process look like": the process-level values, each with where it
// came from.
//
// The synthetic cgroup hierarchy is the point. Doctor has to print the same
// line the startup would install, so it resolves the section through the same
// pure resolver -- pointing that resolver at a temporary hierarchy is what
// makes the derived case observable without a container, and the underivable
// case is the one an operator hits on a bare-metal host.
func TestDoctorReportsTheRuntimeKnobsFromTheContainer(t *testing.T) {
	root := t.TempDir()
	writeCgroupFile(t, root, "cpu.max", "400000 100000")
	writeCgroupFile(t, root, "memory.max", "2147483648") // 2GiB, so 75% is exactly 1.5GiB

	previous := cgroupRoot
	cgroupRoot = root
	t.Cleanup(func() { cgroupRoot = previous })

	app := doctorApp(doctorWorkloadBundles())
	out := runDoctor(t, app, runtimeTestConfigWith(t, time.Second,
		"  runtime:\n    max_procs: auto\n    memory_limit: \"75%\"\n    gc_percent: 20\n"+
			doctorWorkloadConfig("sast"))...)

	assert.Contains(t, out, "max_procs=4 (cgroup)",
		"auto resolves against the container quota, not the host")
	assert.Contains(t, out, "memory_limit=1.5GiB (cgroup, 75%)")
	assert.Contains(t, out, "gc_percent=20 (explicit)")

	// A bare-metal host states nothing to derive from, and doctor must say that
	// rather than print a number nobody configured.
	bare := t.TempDir()
	cgroupRoot = bare
	app = doctorApp(doctorWorkloadBundles())
	out = runDoctor(t, app, runtimeTestConfigWith(t, time.Second, doctorWorkloadConfig("sast"))...)

	assert.Contains(t, out, "max_procs=default (unset, no cgroup cpu quota)")
	assert.Contains(t, out, "memory_limit=default (unset)")
	assert.Contains(t, out, "gc_percent=default (unset)")
}

// TestDoctorReportsManagedTaskBudgetsOnBoundedWorkloadsOnly is the one place
// workloads.<key>.max_goroutines becomes visible.
//
// The field is per workload and discriminating in three directions: a bounded
// hosted workload carries its limit, an unbounded one carries no field at all
// rather than a zero, and a workload this process does not carry carries no
// limit either -- its budget can never be charged, because an unhosted workload
// contributes no plugin to submit anything, so printing it would read as a
// bound that is in force. Printing zero for the unbounded case would make the
// two indistinguishable in exactly the report an operator reads to find out
// which workload is rationed -- and since every workload is unbounded by
// default, that would be the common case drowning the rare one.
func TestDoctorReportsManagedTaskBudgetsOnBoundedWorkloadsOnly(t *testing.T) {
	app := doctorApp(doctorWorkloadBundles())
	out := runDoctor(t, app, runtimeTestConfigWith(t, time.Second,
		"workloads:\n"+
			"  sast:\n    enabled: false\n    max_goroutines: 5\n"+
			"  coderanger:\n    max_goroutines: 4\n")...)
	rows := doctorRows(out)

	assert.Contains(t, rows, "workload coderanger hosted replicas=6 plugins=2 max_goroutines=4",
		"a bounded workload carries its limit")
	assert.Contains(t, rows, "workload webscan hosted replicas=6 plugins=2",
		"an unbounded workload carries no budget field at all")
	assert.NotContains(t, rows, "webscan hosted replicas=6 plugins=2 max_goroutines",
		"unbounded must not be spelled as a limit of zero")
	assert.NotContains(t, rows, "sast not held exclusive replicas=3 plugins=0 max_goroutines",
		"a budget on a workload this process does not carry is never charged, so it is not printed")

	// doctor reports the limit and not the refusal count. A diagnostic run
	// admits no task, so a rejection counter here would read zero for a reason
	// that has nothing to do with the workload's health -- worse than absent,
	// because an operator would read it as "not throttled". The counter belongs
	// to the running process, where the warn log and the metric carry it.
	assert.NotContains(t, out, "rejected=",
		"doctor must not print a counter a diagnostic process cannot observe")
}

// TestDoctorStillReportsDisabledPluginsBesideTheirPath keeps the one section the
// plan cannot attribute by workload from losing the information it does have. A
// Definition the configuration turned off never became an instance, so it
// belongs in a list of its own -- but the reader still needs the path it
// watched, because that is the block they would edit.
func TestDoctorStillReportsDisabledPluginsBesideTheirPath(t *testing.T) {
	on := plugin.Define("switched-on", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Activation: plugin.WhenConfigured("plugins.switched-on")})
	off := plugin.Define("switched-off", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Activation: plugin.WhenConfigured("plugins.switched-off")})

	app := doctorApp([]plugin.Bundle{
		plugin.WorkloadOf("carried", plugin.BundleOf(on, off), plugin.WithReplicas(1)),
	})
	out := runDoctor(t, app, runtimeTestConfigWith(t, time.Second,
		"plugins:\n  switched-on:\n    enabled: true\n")...)

	assert.Contains(t, doctorRows(out), "workload carried hosted replicas=1 plugins=1")
	assert.Contains(t, out, "switched-off")
	assert.Contains(t, out, "plugins.switched-off",
		"a disabled Definition names the section it watched, so the reader knows what to edit")
	assert.Contains(t, out, "activation path plugins.switched-off is not configured",
		"a disabled plugin must say why, not merely that it is off")
}
