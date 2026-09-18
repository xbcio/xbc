package runtime

import (
	"fmt"
	"sort"
	"strings"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

// PlacementSource is plugin.PlacementSource, re-exported so an application
// configures placement through the runtime it already imports.
//
// The vocabulary itself lives in plugin rather than here because a placement
// source must be writable by a module this package cannot import: the core
// dependency closure excludes everything beneath extensions, so a lease-backed
// source is an extension module, and an extension module cannot import runtime
// without inverting the framework's dependency direction.
type PlacementSource = plugin.PlacementSource

// Placement is plugin.Placement, re-exported for the same reason.
type Placement = plugin.Placement

// placementSourceStatic is the label StaticPlacement stamps on its decisions.
// It is a constant rather than a literal so that the default a diagnostic
// prints and the default an application gets cannot drift apart.
const placementSourceStatic = "static"

// StaticPlacement is the default: the hosted set is exactly the declared
// workloads configuration enables. It contacts nothing and never fails.
//
// It is the whole placement decision for a deployment that assigns roles in its
// configuration, and it is also the veto a lease source must apply: a workload
// with "workloads.<key>.enabled: false" is refused by every process shape,
// whatever decided the rest of the set.
func StaticPlacement() PlacementSource { return staticPlacement{} }

type staticPlacement struct{}

func (staticPlacement) Resolve(request plugin.PlacementRequest) (plugin.Placement, error) {
	hosted := make([]plugin.WorkloadKey, 0, len(request.Workloads))
	for _, workload := range request.Workloads {
		if request.Admits(workload.Key) {
			hosted = append(hosted, workload.Key)
		}
	}
	return plugin.Placement{Source: placementSourceStatic, Hosted: hosted}, nil
}

// WithPlacement selects how this process decides which declared workloads it
// hosts. Omitted, the decision is StaticPlacement.
//
// The source is consulted once per run, before the assembly plan is built. An
// application that owns no placement infrastructure leaves it out, and an
// application whose roles are assigned by a lease passes the one source that
// reads the lease.
func WithPlacement(source PlacementSource) Option {
	return func(options *appOptions) error {
		// A nil source is rejected rather than read as "the default": the
		// caller wrote WithPlacement because it meant to name one, and a
		// typed-nil interface that silently fell back to static placement
		// would make the process carry every enabled workload while its
		// composition root says otherwise.
		if source == nil {
			return fmt.Errorf("xbc: WithPlacement requires a non-nil PlacementSource; omit the option to accept StaticPlacement")
		}
		options.placement = source
		options.hasPlacement = true
		return nil
	}
}

// placement returns the configured source, or the default.
func (a *App) placement() PlacementSource {
	if a.placementSource == nil {
		return StaticPlacement()
	}
	return a.placementSource
}

// resolvePlacement settles this process's hosted set, once, before the assembly
// plan exists.
//
// The order is the point: the hosted set is an input to BuildPlan rather than a
// decision taken while constructing, so a workload this process does not carry
// contributes no Definition, no factory, no configuration binding and no route
// to the graph at all. Everything downstream -- construction, migration,
// startup, the traffic gate, the reverse unwind -- then needs no notion of a
// workload whatsoever.
//
// It runs for every command that builds a plan, doctor included, because a
// diagnostic reporting a different hosted set than the one a run would use
// would be worse than no diagnostic. It constructs nothing: a source receives
// declared workloads and a configuration predicate, and the one source that
// talks to anything is deliberately not a plugin, so there is no plugin graph
// for it to need.
//
// The source's answer is checked before it is returned, so an answer that
// cannot shape a process -- one that names no source, an undeclared key, a key
// listed twice, an exclusive workload beside another -- fails here rather than
// shaping a process that would then have to be undone.
func (a *App) resolvePlacement() (plugin.Placement, error) {
	sections, err := assembly.ReadWorkloadSections(a.bundles, a.env)
	if err != nil {
		return plugin.Placement{}, err
	}
	workloads := make([]plugin.Workload, len(sections))
	enabled := make(map[plugin.WorkloadKey]bool, len(sections))
	for index, section := range sections {
		workloads[index] = section.Workload
		enabled[section.Workload.Key] = section.Enabled
	}
	source := a.placement()
	placement, err := source.Resolve(plugin.PlacementRequest{
		Workloads: workloads,
		Enabled:   func(key plugin.WorkloadKey) bool { return enabled[key] },
	})
	if err != nil {
		return plugin.Placement{}, err
	}
	if err := validatePlacement(placement, enabled); err != nil {
		return plugin.Placement{}, err
	}
	if err := validateExclusiveHosting(placement.Hosted, workloads); err != nil {
		return plugin.Placement{}, err
	}
	return placement, nil
}

// validatePlacement checks a decision before anything is built from it.
//
// A source is application-supplied code, so its answer is not taken on trust: a
// hosted key nothing declares would silently drop every workload from the plan
// -- the process would carry nothing but unowned plugins and still report ready
// -- and a key listed twice would make a count disagree with the set assembly
// actually used. Both are rejected here rather than tolerated, because by the
// time assembly could notice, the decision has already shaped the process.
//
// An unattributed answer is refused for the same reason, and it is the one that
// fails open. Assembly reads a Placement with no Source and no hosted keys as
// "nobody consulted a source", which hosts every workload the configuration
// enables -- the compatibility path a caller with no placement source needs.
// Once a source has been consulted that reading is wrong: a source that forgot
// to name itself, or that meant "carry nothing", would have its answer widened
// into "carry everything", making the process shape non-deterministic and, in a
// lease deployment, failing open on capacity. A source that hosts nothing says
// so by naming itself and hosting no key, which is honoured exactly.
func validatePlacement(placement plugin.Placement, declared map[plugin.WorkloadKey]bool) error {
	if strings.TrimSpace(placement.Source) == "" {
		return fmt.Errorf("xbc: placement source returned a decision with an empty Source, which cannot be told apart from the absence of a decision and would host every enabled workload; name whatever settled the set, and host no key to carry none")
	}
	seen := make(map[plugin.WorkloadKey]bool, len(placement.Hosted))
	for _, key := range placement.Hosted {
		switch {
		case !declared[key]:
			return fmt.Errorf("xbc: placement source %q hosted workload %q, which this composition does not declare", placement.Source, key)
		case seen[key]:
			return fmt.Errorf("xbc: placement source %q hosted workload %q twice; a process carries a workload or it does not", placement.Source, key)
		}
		seen[key] = true
	}
	return nil
}

// validateExclusiveHosting refuses a hosted set that puts a workload declared
// exclusive in a process carrying anything else.
//
// The rule is about co-residence rather than about exclusivity: an exclusive
// workload alone in its process is exactly the shape it asked for, and one
// ordinary workload beside another is the shape placement exists to make
// possible. What is refused is the combination, because the process-wide side
// effect an exclusive declaration exists to contain -- a global GC target, a
// process-wide memory limit, a pool sized from the whole host -- lands on every
// process-mate that cannot opt out of it. Splitting one process into two is a
// deployment change; living with a process-mate's tuning is not something a
// plugin can do at all.
//
// It runs here, before BuildPlan, rather than being left to assembly: a process
// that is going to be refused must not build a graph first, and by the time
// assembly could notice, the hosted set has already been turned into the set of
// Definitions that exist.
//
// The message names both sides. "An exclusive workload shares a process" leaves
// the operator to work out which of the two declarations to move, and the fix
// differs depending on which one it is.
func validateExclusiveHosting(hosted []plugin.WorkloadKey, declared []plugin.Workload) error {
	if len(hosted) < 2 {
		return nil
	}
	exclusive := make(map[plugin.WorkloadKey]bool, len(declared))
	for _, workload := range declared {
		exclusive[workload.Key] = workload.Exclusive
	}
	// The set is reported in key order rather than in whatever order the source
	// produced it, so two processes that disagree about nothing still print the
	// same message.
	keys := append([]plugin.WorkloadKey(nil), hosted...)
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, key := range keys {
		if !exclusive[key] {
			continue
		}
		others := make([]string, 0, len(keys)-1)
		for _, other := range keys {
			if other != key {
				others = append(others, fmt.Sprintf("%q", other))
			}
		}
		return fmt.Errorf(
			"xbc: workload %q requires a process of its own, but this process also hosts %s\n  an exclusive workload has process-wide side effects that no process-mate can be protected from; set workloads.%s.enabled to false here, or move %q to a process that carries nothing else",
			key, strings.Join(others, ", "), key, key)
	}
	return nil
}

// workloadLabels renders a hosted set for a log line. An empty set is spelled
// out rather than left blank: a process that carries no workload is a
// deliberate shape -- it is the standby that owns only the unowned plugins --
// and a blank field would read as a field nobody filled in.
func workloadLabels(hosted []plugin.WorkloadKey) []string {
	if len(hosted) == 0 {
		return []string{"(none)"}
	}
	labels := make([]string, len(hosted))
	for index, key := range hosted {
		labels[index] = key.String()
	}
	return labels
}
