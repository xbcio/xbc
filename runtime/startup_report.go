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
		{"bootstrap", p.bootstrap},
		{"planning", p.planning},
		{"construct", p.construct},
		{"migrate", p.migrate},
		{"start", p.start},
		{"traffic", p.openTraffic},
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

func joinTimings(entries []labelledDuration) string {
	parts := make([]string, len(entries))
	for index, entry := range entries {
		parts[index] = entry.label + " " + entry.duration.String()
	}
	return strings.Join(parts, ", ")
}
