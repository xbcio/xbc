package plugin

import (
	"fmt"
	"strings"

	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

// WorkloadKey is the stable, configuration-facing identity of a workload: a
// named group of Definitions a process carries as a unit or not at all. It is
// never derived from a Go package path or type name.
type WorkloadKey = pluginmodel.WorkloadKey

// Workload is one declared workload's placement identity together with the
// cluster-level placement constraints it imposes.
type Workload = pluginmodel.Workload

// WorkloadOption is one placement constraint of a workload declaration. It is
// sealed: applyWorkload is unexported, so the only options that exist are the
// two constructors below. A workload's placement is a cluster-level promise
// rather than a per-process preference, and sealing the set is what keeps an
// application from inventing a third one.
type WorkloadOption interface {
	applyWorkload(*pluginmodel.Workload)
}

type workloadOption func(*pluginmodel.Workload)

func (option workloadOption) applyWorkload(workload *pluginmodel.Workload) { option(workload) }

// WithExclusiveProcess declares that a process holding this workload holds no
// other workload.
//
// It is for a workload with process-wide side effects -- tuning a global GC
// target, setting a process-wide memory limit, sizing a shared pool -- which
// nothing sharing its process can be protected from. The reason is not that
// the workload is heavy: a heavy workload expresses that as a replica count and
// a task budget instead, neither of which forces it into a process of its own.
func WithExclusiveProcess() WorkloadOption {
	return workloadOption(func(workload *pluginmodel.Workload) { workload.Exclusive = true })
}

// WithReplicas declares how many processes may hold this workload at once. At
// least one is required.
//
// The count is a placement declaration rather than an enforcement this package
// carries out: the PlacementSource is what decides which processes hold the
// workload, so it is the source that reads the number. A source that manages
// slots enforces it -- a process competes for exactly one slot of each workload
// it carries, making replicas the true maximum concurrency of that workload
// across the cluster. The default StaticPlacement assigns no slots and ignores
// the count, hosting every workload the configuration enables; a deployment
// that needs the bound met names a source that reads it.
func WithReplicas(n int) WorkloadOption {
	if n <= 0 {
		panic(fmt.Sprintf("xbc: plugin.WithReplicas requires a positive replica count, got %d; a workload with no replica can never be carried", n))
	}
	return workloadOption(func(workload *pluginmodel.Workload) { workload.Replicas = n })
}

// WorkloadOf declares one workload: a stable key, the Bundle of Definitions it
// owns, and its cluster-level placement constraints.
//
// It declares no Definition of its own. Like BundleOf it is pure composition
// data -- evaluating it creates no resource, starts no goroutine, and mutates
// no process state -- so a package can evaluate its workload Bundle once at
// package scope and return it from Bundle():
//
//	// Package sast is the application's static analysis workload.
//	package sast
//
//	// Key is this workload's stable placement and configuration identity.
//	const Key plugin.WorkloadKey = "sast"
//
//	var bundle = plugin.WorkloadOf(
//		Key,
//		plugin.BundleOf(dispatcherDefinition, workerDefinition),
//		plugin.WithExclusiveProcess(),
//		plugin.WithReplicas(3),
//	)
//
//	func Bundle() plugin.Bundle { return bundle }
//
// The composition root then selects it exactly like any other Bundle. Every
// occurrence it gathers is tagged with key, which is what lets assembly decide
// whether a process carries those Definitions at all.
//
// A workload owns Definitions, never another workload: every member Definition
// must be able to live in a process that carries this workload and no other, so
// nesting one workload inside another has no meaning to express. A member whose
// Definition lives in another module and cannot be gathered here declares its
// membership with Options[P].Workload instead.
func WorkloadOf(key WorkloadKey, plugins Bundle, options ...WorkloadOption) Bundle {
	if err := key.Validate(); err != nil {
		// The validator already states the rule; dropping its shared prefix
		// keeps the panic one sentence attributed to this constructor.
		panic("xbc: plugin.WorkloadOf " + strings.TrimPrefix(err.Error(), "xbc: "))
	}
	erased := pluginmodel.Bundle(plugins)
	// The occurrences are inspected before the declarations so that the
	// ordinary mistake -- passing another workload's Bundle, which tags its
	// members -- is reported as the tagged membership it actually is. The
	// declaration check then covers the one Bundle that declares a workload
	// without tagging anything: an empty one.
	for _, entry := range pluginmodel.BundleEntries(erased) {
		if entry.Workload != "" {
			panic(fmt.Sprintf("xbc: plugin.WorkloadOf for workload %q was given a Bundle whose occurrences already belong to workload %q; a Definition belongs to at most one workload", key, entry.Workload))
		}
	}
	for _, nested := range pluginmodel.BundleWorkloads(erased) {
		panic(fmt.Sprintf("xbc: plugin.WorkloadOf for workload %q was given a Bundle that declares workload %q; compose Definitions here and select the nested workload at the composition root", key, nested.Key))
	}

	workload := pluginmodel.Workload{Key: key, Replicas: 1}
	for index, option := range options {
		if option == nil {
			panic(fmt.Sprintf("xbc: plugin.WorkloadOf item %d is nil", index))
		}
		option.applyWorkload(&workload)
	}
	return Bundle(pluginmodel.AssignWorkload(erased, workload))
}

// BundleWorkloads returns the workloads bundle declares, sorted by key, with a
// key declared more than once reported from its first occurrence.
func BundleWorkloads(bundle Bundle) []Workload {
	return pluginmodel.BundleWorkloads(pluginmodel.Bundle(bundle))
}
