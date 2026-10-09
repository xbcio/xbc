package runtime

import (
	"fmt"
	"strings"
	"time"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin/assembly"
)

// reportDisabled lists declared-but-not-enabled Definitions with the reason
// each one stayed off.
//
// Being disabled is normal and not a warning, but "I selected it and nothing
// happened" is otherwise indistinguishable from "I selected it and it
// silently failed", and this line is what separates the two. Only keys,
// paths and reasons are printed -- never a configured value.
func (a *App) reportDisabled(plan *assembly.Plan) {
	disabled := plan.DisabledDetail()
	if len(disabled) == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "xbc: disabled plugins (%d)", len(disabled))
	for _, entry := range disabled {
		fmt.Fprintf(&b, "\n  %s — %s", entry.Key, entry.Reason)
	}
	b.WriteString("\n  → Being disabled is not an error; run the doctor subcommand for the full configuration picture")
	a.log().Info(b.String())
}

// reportSaturatedWorkloads warns about every bounded workload whose budget is
// fully held once the last Start hook has returned.
//
// The warning exists because the admission half of the budget is silent by
// construction: an admission waits for a unit rather than refusing, so a
// workload whose own process-lifetime tasks have filled the limit never runs
// the queue delivery, the pooled task or the cron invocation that was meant to
// charge it, and nothing but a wait nobody is watching says so. The refusal
// half already warns where it happens (taskRuntime.submit). The end of the
// Start phase is when the reading is at its most telling: submission of a
// managed task is admitted only inside a Start hook, so the workload's own
// start-up work has all been submitted, and the queue workers and cron runners
// that charge the admission half are still waiting on a traffic gate that has
// not opened.
//
// It warns rather than fails: the counter cannot tell a process-lifetime task
// from one that is merely still running -- a Start hook may have spawned
// transient work into a workload-scoped pool, which waits for no gate -- so a
// workload with nothing left is unusual rather than illegal, and the message
// says what was observed instead of asserting a hang that a task ending a
// moment later would disprove.
func (a *App) reportSaturatedWorkloads() {
	for _, report := range a.tasks.workloadBudgets() {
		if report.Running < report.Limit {
			continue
		}
		a.log().Warn("xbc: workload goroutine budget fully held at the end of start; an admission for it waits until a held unit comes free",
			"workload", report.Workload.String(),
			"limit", report.Limit,
			"running", report.Running)
	}
}

func (a *App) reportStarted(instances []*assembly.Instance, migrate bool) {
	labels := make([]string, len(instances))
	for index, instance := range instances {
		labels[index] = instance.Identity().String()
	}
	a.log().Info("xbc: application traffic gate released",
		"instances", len(instances),
		"order", strings.Join(labels, ","),
		"migration", migrate,
		"startup", a.startup.total.String(),
	)
}

// reportValidated states how long the validate command took and how many
// instances it checked. It is reportStarted's counterpart for the command that
// ends at validation rather than at a released traffic gate: the same anchor
// measures it -- the moment argument parsing succeeded, before any of the work
// being timed -- so an operator reads the two numbers the same way.
//
// It is emitted before the unwind, because what it reports is the validation,
// not the shutdown that follows it; a stop that fails afterwards is reported as
// its own error by the caller.
func (a *App) reportValidated(instances []*assembly.Instance) {
	a.log().Info("xbc: application validation finished",
		"instances", len(instances),
		"validation", a.validation.String(),
	)
}

// startupTiming is how long this App took to reach servable, and where that
// time went.
type startupTiming struct {
	total  time.Duration
	phases startupPhases
}

// startupPhases is how long each ordered startup phase took. The phases are
// fixed in number and named by the framework, so reporting all of them costs
// the same whether an application selected three plugins or forty.
type startupPhases struct {
	bootstrap   time.Duration
	planning    time.Duration
	construct   time.Duration
	migrate     time.Duration
	start       time.Duration
	openTraffic time.Duration
}

func (p startupPhases) String() string {
	return joinTimings([]labelledDuration{
		{phaseBootstrap, p.bootstrap},
		{phasePlanning, p.planning},
		{phaseConstruct, p.construct},
		{phaseMigrate, p.migrate},
		{phaseStart, p.start},
		{phaseTraffic, p.openTraffic},
	})
}

// reportStartupTimings breaks a slow boot down to the plugin and the stage.
//
// The released-gate line above always states how long the boot took, because
// that is one field and every operator wants it. This breakdown grows with the
// number of selected plugins, so it lands at debug: a boot is reproducible on
// demand, and a table nobody reads on every restart is how operators learn to
// skip the report that matters. Nothing here is a configured value -- only
// identities, stage names and durations.
func (a *App) reportStartupTimings(instances []*assembly.Instance) {
	if !a.log().Enabled(log.DebugLevel) {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "xbc: startup timings, total %s", a.startup.total)
	fmt.Fprintf(&b, "\n  phases: %s", a.startup.phases)
	for _, instance := range instances {
		timings := instance.Timings()
		if len(timings) == 0 {
			continue
		}
		stages := make([]labelledDuration, len(timings))
		for index, timing := range timings {
			stages[index] = labelledDuration{string(timing.Stage), timing.Duration}
		}
		fmt.Fprintf(&b, "\n  %-28s %s", instance.Identity().String(), joinTimings(stages))
	}
	a.log().Debug(b.String())
}

type labelledDuration struct {
	label    string
	duration time.Duration
}

// reportValidationTimings breaks a slow validation down to the plugin stage,
// the way reportStartupTimings does for a boot, and lands at debug for the same
// reason: the breakdown grows with the number of selected plugins while the
// line above already states the one number every operator wants. The
// always-on number is what makes it safe to keep this one at debug -- a
// validation that got slow shows up as a longer total whatever the log level,
// and the slow-startup watchdog names the plugin still holding the phase when
// it crosses the configured threshold.
//
// Preflight is the only stage listed. Configuration and construction are the
// work validate shares with every other command, and the boot report already
// measures those; what is specific to this command is the hook that a
// composition need not declare at all, so an instance without one contributes
// no line and no zero.
func (a *App) reportValidationTimings(instances []*assembly.Instance) {
	if !a.log().Enabled(log.DebugLevel) {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "xbc: validation timings, total %s", a.validation)
	for _, instance := range instances {
		var preflight time.Duration
		found := false
		for _, timing := range instance.Timings() {
			if timing.Stage != assembly.StagePreflight {
				continue
			}
			preflight, found = timing.Duration, true
		}
		if !found {
			continue
		}
		fmt.Fprintf(&b, "\n  %-28s %s", instance.Identity().String(),
			joinTimings([]labelledDuration{{string(assembly.StagePreflight), preflight}}))
	}
	a.log().Debug(b.String())
}

func joinTimings(entries []labelledDuration) string {
	parts := make([]string, len(entries))
	for index, entry := range entries {
		parts[index] = entry.label + " " + entry.duration.String()
	}
	return strings.Join(parts, ", ")
}
