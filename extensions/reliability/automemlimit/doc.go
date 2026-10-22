// Package automemlimit installs the process soft memory limit from the
// container's cgroup memory limit, which is the limit the Go garbage collector
// sizes itself against.
//
// # Why this is a plugin
//
// The soft memory limit applies to every allocation in the binary, so it is a
// process-level decision. The framework already owns the knobs it can derive
// from xbc.runtime (GOMAXPROCS, GOGC, and an explicit or percentage memory
// limit), and a component that changed one from inside itself would be tuning
// the budget of the components beside it. This package exists because a
// container limit is the case the framework cannot decide for an application:
// deriving it means reading cgroup v1 and v2 hierarchies, which is a
// dependency the framework core deliberately does not carry. Selecting this
// Bundle is therefore an explicit application-level decision; the package is
// not part of any transport prelude or starter.
//
// # One owner of the process limit
//
// This plugin and xbc.runtime.memory_limit must not both install a limit. When
// the process already has a non-default soft memory limit at Init -- because
// the framework installed one from xbc.runtime.memory_limit, or because the
// application set one itself -- startup fails and names both ways out: set
// xbc.runtime.memory_limit to 0, or do not select this plugin. It never
// overwrites a limit silently, because a last-writer-wins race between two
// policy owners is exactly the failure that is hardest to diagnose from the
// outside.
//
// # Priority
//
// The environment decides first, mirroring the automemlimit library's own
// contract:
//
//   - GOMEMLIMIT, when present, is the highest priority: the plugin installs
//     nothing and leaves the value alone.
//   - AUTOMEMLIMIT=off disables the plugin's installation.
//   - AUTOMEMLIMIT=<ratio> overrides the configured ratio at install time.
//   - plugins.automemlimit.ratio applies when neither variable decides.
//
// # Platforms
//
// On a system without cgroups, or in a Linux process with no cgroup memory
// controller, there is no limit to derive. That is a normal, non-fatal outcome:
// the plugin logs one warning and the process starts with whatever limit it
// already had. Only a genuine failure to read a cgroup the process does have
// fails startup.
//
// # Refresh
//
// With refresh_interval set to a positive duration, Start submits one managed
// task that re-reads the container limit every interval and re-applies it. The
// task is owned by the framework, so shutdown cancellation ends it, and a
// failed refresh is logged and retried on the next tick. The default 0s
// installs the limit once during Init and submits no task at all.
//
// The limit is deliberately not restored on shutdown: it is process-global,
// any restore could only run on the exit path, and no other component can rely
// on a value that disappears with the process.
//
// # Diagnostics
//
// doctor resolves and prints the plan without constructing plugins, so it
// neither installs a limit nor detects a conflict. validate constructs the
// plugins a boot would construct, so it installs the limit in its own process
// and reports the conflict a boot would report. The effective-value line of
// xbc.runtime keeps its own view of the framework knob and therefore still
// reads memory_limit=default (unset); the plugin's Init log line reports the
// limit it actually installed.
//
// # Usage
//
// Compose the Bundle explicitly, or blank-import the leaf autoload adapter
// from an executable:
//
//	app, err := xbc.New(xbc.WithBundles(
//		automemlimit.Bundle(),
//	))
//
//	plugins:
//	  automemlimit:
//	    ratio: 0.9
//	    fallback: none
//
// The default ratio matches the library's: 90% of the container limit. Set
// fallback to "system" only on bare metal, because that fallback also hides
// cgroup read errors. min_bytes raises the result when the ratio would leave
// too little headroom; it cannot know whether it exceeds the container limit,
// so keeping it below that limit is a deployment responsibility.
package automemlimit
