package runtime

import (
	"fmt"
	"io"
	"strings"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

// doctorSubcommand names the read-only diagnostic command.
const doctorSubcommand = "doctor"

// unownedGroup is the row doctor prints for the Definitions that belong to no
// workload. It is spelled as a row of the same list rather than as a separate
// section because the question it answers -- "does this process carry anything
// outside its workloads" -- is the same question the workload rows answer, and
// a process whose composition declares no workload at all still has exactly one
// honest answer to it.
const unownedGroup = "unowned"

// doctorInstanceLabels aligns the four labelled lines of one instance block.
// The widest of them is "selected at", so the value column starts one space
// after it and the whole block can be read down rather than across. The scale
// is the label alone: the format adds the separating space, so a label that is
// exactly this wide still leaves one.
const doctorInstanceLabelWidth = len("selected at")

// reportDoctor writes the outcome of a complete, read-only assembly, grouped by
// the placement question an operator is actually asking: where this process's
// hosting decision came from, which workloads it carries and why, which plugins
// those turned on, which section each instance binds, where each instance was
// selected, what feeds its declared inputs, why the remaining Definitions
// stayed off, and which sources contributed.
//
// Everything printed here is a path, an identity, a contract type name or a
// source label. No configured value is ever written, which is what makes the
// command safe to run against a production environment full of secrets.
// Sections are separated by a blank line so a later command can append its own
// without reformatting the ones above it.
//
// Nothing here constructs anything. Every value comes from the plan, from the
// settings and from the pure resolvers beside them, which is the whole reason
// doctor can be run against a configuration whose side effects are exactly what
// an operator is unsure about.
func (a *App) reportDoctor(plan *assembly.Plan, migrate bool) {
	out := a.diagnostics()

	fmt.Fprintln(out, "xbc doctor")
	fmt.Fprintln(out)

	a.reportDoctorConfiguration(out)

	order := plan.Order()
	disabled := plan.DisabledDetail()
	fmt.Fprintln(out, "plugins")
	fmt.Fprintf(out, "  declared %d, enabled instances %d, disabled %d\n",
		plan.DefinitionCount(), len(order), len(disabled))
	fmt.Fprintln(out)

	a.reportDoctorPlacement(out, plan)
	a.reportDoctorRuntime(out)
	a.reportDoctorWorkloads(out, plan, order)
	a.reportDoctorDisabled(out, disabled)

	fmt.Fprintln(out, "migration")
	fmt.Fprintf(out, "  this run would migrate: %t\n", migrate)
}

// reportDoctorConfiguration names where configuration came from and which roots
// were declared. The roots line is also the answer to "why was that key
// accepted": ownership is decided by this declared set and nothing else.
func (a *App) reportDoctorConfiguration(out io.Writer) {
	fmt.Fprintln(out, "configuration")
	sources := a.env.Sources()
	if len(sources) == 0 {
		fmt.Fprintln(out, "  sources  (none; every value comes from a declared default)")
	} else {
		fmt.Fprintf(out, "  sources  %s\n", strings.Join(sources, ", "))
	}
	fmt.Fprintf(out, "  roots    %s\n", strings.Join(a.env.Roots(), ", "))
	fmt.Fprintln(out)
}

// reportDoctorPlacement names the decision the whole report hangs off: which
// source made it, who claimed it, what was claimed, and anything the source
// chose to explain.
//
// The notes are the only place an operator can see why a source declined a
// workload it could have taken -- a lease source knows which slots were already
// held and doctor does not -- so they are printed verbatim rather than
// summarised, one per line, and their absence is spelled out rather than left
// blank.
func (a *App) reportDoctorPlacement(out io.Writer, plan *assembly.Plan) {
	placement := plan.Placement()
	fmt.Fprintln(out, "placement")
	fmt.Fprintf(out, "  source   %s\n", placement.Source)
	if placement.Holder == "" {
		fmt.Fprintln(out, "  holder   (none; only a source that records a claimant names one)")
	} else {
		fmt.Fprintf(out, "  holder   %s\n", placement.Holder)
	}
	fmt.Fprintf(out, "  hosted   %s\n", strings.Join(workloadLabels(placement.Hosted), ", "))
	if len(placement.Notes) == 0 {
		fmt.Fprintln(out, "  notes    (none)")
	} else {
		fmt.Fprintln(out, "  notes")
		for _, note := range placement.Notes {
			fmt.Fprintf(out, "    %s\n", note)
		}
	}
	fmt.Fprintln(out)
}

// reportDoctorRuntime answers the other half of "what does this process look
// like at runtime": the process-level knobs, with where each effective value
// came from.
//
// It resolves the section through the same pure resolver and prints it with the
// same formatter the startup line uses, so a diagnosis can never disagree with
// the boot it is diagnosing. That is also why doctor must not install them: it
// is run precisely when changing the GOMAXPROCS and memory limits of the
// process under inspection may be unsafe.
func (a *App) reportDoctorRuntime(out io.Writer) {
	fmt.Fprintln(out, "runtime")
	knobs, err := resolveRuntimeKnobs(a.settings.Runtime, cgroupRoot)
	if err != nil {
		// Unreachable by construction -- bootstrap resolved this same section
		// from this same root before the plan existed, and a section that
		// cannot be resolved fails the command there -- but reported rather
		// than skipped, because a silently missing line would be
		// indistinguishable from a line with nothing in it.
		fmt.Fprintf(out, "  unresolved: %v\n", err)
	} else {
		fmt.Fprintf(out, "  %s\n", knobs.describe())
	}
	fmt.Fprintln(out)
}

// workloadBudgets indexes the managed-task budgets by workload, so a workload
// row can carry its own budget without the reader matching two lists up.
//
// It reads the limits rather than the counters, and that distinction is the
// point: doctor is a single-use diagnostic that admits no task, so every
// rejection counter it could print would read zero for a reason that has
// nothing to do with the workload being healthy. The configured limit is
// genuinely doctor's to report -- it is derived from the very configuration
// doctor is already describing -- while "is this workload being throttled" can
// only be answered by the running process, through the warn log and the
// counter that process keeps. It touches nothing else: no runtime is started,
// no task is submitted, and no lock is held beyond the instant of the read.
func (a *App) workloadBudgets() map[plugin.WorkloadKey]workloadBudgetReport {
	reports := a.tasks.workloadBudgets()
	if len(reports) == 0 {
		return nil
	}
	byWorkload := make(map[plugin.WorkloadKey]workloadBudgetReport, len(reports))
	for _, report := range reports {
		byWorkload[report.Workload] = report
	}
	return byWorkload
}

// reportDoctorWorkloads groups the graph by placement. Every declared workload
// gets a row whether or not this process carries it, followed by the instances
// it contributed; everything belonging to no workload is gathered under the
// unowned row so that "what does this process carry" is answered by reading
// down the left edge.
//
// An unhosted workload is listed rather than omitted. "This process does not
// carry sast" is the answer an operator is looking for when a role is missing,
// and a workload absent from the list could not be told apart from one the
// composition never declared.
//
// A workload that declares a managed-task budget carries it on its row, but
// only when this process actually hosts it. The budget is charged against
// submissions from the workload's own plugins, and an unhosted workload
// contributes no plugin to submit anything -- so printing its limit beside
// "not held" would read as a bound that is in force when nothing can consume
// it.
//
// A hosted workload with no bound gets no such field rather than one reading
// zero: "unbounded" and "bounded at zero" are different answers, only one of
// them is configurable, and a report that printed the common case would make
// the rare one unreadable.
func (a *App) reportDoctorWorkloads(out io.Writer, plan *assembly.Plan, order []plugin.Identity) {
	workloads := plan.Workloads()
	budgets := a.workloadBudgets()

	// The unowned set is the graph order minus every workload's own identities.
	// Deriving it from the plan rather than from the Bundles is what keeps this
	// report describing the graph that was actually built: a Definition the
	// configuration disabled has no identity here at all, and belongs in the
	// disabled list rather than under a group.
	unowned := make([]plugin.Identity, 0, len(order))
	for _, identity := range order {
		if _, grouped := plan.WorkloadOf(identity); !grouped {
			unowned = append(unowned, identity)
		}
	}

	width := len(unownedGroup)
	for _, workload := range workloads {
		if label := doctorWorkloadLabel(workload.Workload.Key); len(label) > width {
			width = len(label)
		}
	}

	for _, workload := range workloads {
		fields := []string{doctorHostedLabel(workload.Hosted)}
		if workload.Workload.Exclusive {
			fields = append(fields, "exclusive")
		}
		fields = append(fields,
			fmt.Sprintf("replicas=%d", workload.Workload.Replicas),
			fmt.Sprintf("plugins=%d", len(workload.Identities)))
		if workload.Hosted {
			if budget, bounded := budgets[workload.Workload.Key]; bounded {
				fields = append(fields, fmt.Sprintf("max_goroutines=%d", budget.Limit))
			}
		}
		fmt.Fprintf(out, "%-*s  %s\n", width, doctorWorkloadLabel(workload.Workload.Key), strings.Join(fields, "  "))
		a.reportDoctorInstances(out, plan, workload.Identities)
	}

	fmt.Fprintf(out, "%-*s  plugins=%d\n", width, unownedGroup, len(unowned))
	a.reportDoctorInstances(out, plan, unowned)
}

// doctorWorkloadLabel is the left-edge name of one workload's row. The
// "workload" prefix is what keeps a row from being read as a configuration key:
// the key that appears here is a declaration, and editing it means editing Go
// rather than the file doctor was pointed at.
func doctorWorkloadLabel(key plugin.WorkloadKey) string {
	return "workload " + key.String()
}

// doctorHostedLabel spells out both sides of the hosting decision rather than
// printing a bare flag, because the row a reader scans is often the only place
// they learn that an unhosted workload still exists.
func doctorHostedLabel(hosted bool) string {
	if hosted {
		return "hosted"
	}
	return "not held"
}

// reportDoctorInstances writes the instances one group contributed, in graph
// order, each with the section it binds, where its values came from, the
// composition site that selected it, and what feeds its declared inputs.
//
// The origin line keeps the wording it has always had ("from env", "from
// file …"). The "configuration" section above states the same fact globally,
// but the per-instance spelling is what an operator reads while asking about
// one plugin, and it is the one thing that distinguishes an environment-only
// activation from a file-defined one.
//
// A group with nothing under it says so. An empty group and a group whose
// instances failed to print look identical otherwise, and the empty case is a
// real answer -- a hosted workload whose plugins are all disabled, or a process
// carrying only workloads.
func (a *App) reportDoctorInstances(out io.Writer, plan *assembly.Plan, identities []plugin.Identity) {
	if len(identities) == 0 {
		fmt.Fprintln(out, "  (no instances)")
		fmt.Fprintln(out)
		return
	}
	for _, identity := range identities {
		fmt.Fprintln(out, "  "+identity.String())
		path := plan.InstanceConfigPath(identity)
		fmt.Fprintf(out, "    %-*s %s\n", doctorInstanceLabelWidth, "config", path)
		fmt.Fprintf(out, "    %-*s from %s\n", doctorInstanceLabelWidth, "origin",
			describeOrigins(a.env.OriginsUnder(path)))
		fmt.Fprintf(out, "    %-*s %s\n", doctorInstanceLabelWidth, "selected at",
			plan.InstanceSelectedAt(identity))
		edges := plan.InstanceInputs(identity)
		if len(edges) == 0 {
			fmt.Fprintln(out, "    no declared inputs")
			continue
		}
		for _, edge := range edges {
			fmt.Fprintf(out, "    %-*s %-9s %-38s %s\n", doctorInstanceLabelWidth, "requires",
				edge.Query, edge.Contract, describeProducers(edge))
		}
	}
	fmt.Fprintln(out)
}

// reportDoctorDisabled lists the Definitions the configuration turned off, each
// with the path it watched and the reason it stayed off.
//
// They stay in one list rather than being distributed among the workload groups
// because that is all the plan can attribute them by: a Definition whose
// configuration disabled it never became an instance, and the plan records
// workload membership per instance. The path is printed beside the reason so a
// reader can still see which section to edit and which root it sits under.
func (a *App) reportDoctorDisabled(out io.Writer, disabled []assembly.DisabledDefinition) {
	fmt.Fprintln(out, "disabled plugins")
	if len(disabled) == 0 {
		fmt.Fprintln(out, "  (none)")
	} else {
		for _, entry := range disabled {
			fmt.Fprintf(out, "  %-28s %-28s %s\n", entry.Key.String(), entry.Path, entry.Reason)
		}
	}
	fmt.Fprintln(out)
}

// describeProducers names what wiring bound to one declared input. An input
// that bound nothing is spelled out instead of left blank, because an optional
// or collecting query resolving to nothing is legal, silent, and identical from
// the outside to one that resolved: doctor is the only place an operator can
// find out that the extension they believed was attached never was.
func describeProducers(edge assembly.InputEdge) string {
	if len(edge.Producers) == 0 {
		return "unsatisfied: no enabled plugin exports it"
	}
	labels := make([]string, len(edge.Producers))
	for index, producer := range edge.Producers {
		labels[index] = producer.String()
	}
	return "from " + strings.Join(labels, ", ")
}

// describeOrigins renders where a section's values came from. A section nobody
// configured is reported as such rather than left blank, so that an ENV-only
// activation is visible at a glance.
func describeOrigins(origins []string) string {
	if len(origins) == 0 {
		return "declared defaults only"
	}
	return strings.Join(origins, ", ")
}

// diagnostics is where read-only command output goes. Tests replace it; the
// process adapter owns the real stream.
func (a *App) diagnostics() io.Writer {
	if a.out != nil {
		return a.out
	}
	return stdout
}
