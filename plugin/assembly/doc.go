// Package assembly plans and constructs instances from plugin definitions.
//
// It is low-level framework infrastructure used by the XBC runtime. Applications
// should compose plugin.Bundle values through the root xbc package instead.
//
// # Placement
//
// Planning starts from a hosting decision rather than producing one. The
// runtime settles which workloads this process carries, passes it as
// PlanOptions.Placement, and a Definition belonging to a workload the process
// does not carry is dropped before it can enter the graph. It is then absent
// rather than disabled: it has no factory call, no configuration binding, no
// contract registration, and no entry in DisabledDetail. Plan.Workloads reports
// the decision itself, including the workloads left out, and that report is the
// only place a workload this process does not carry is visible at all.
//
// # How the hosted set layers with plugin enablement
//
// Three decisions turn a declared Definition into a constructed instance, and
// they are applied in this order because each answers a question the next one
// cannot be asked without:
//
//  1. The hosted set decides whether the Definition is part of this process at
//     all. A workload the deployment excluded, or one a placement source did not
//     claim, takes every one of its Definitions out here, whatever their own
//     "plugins.<key>.enabled" says. This is the only stage that removes a
//     Definition from consideration entirely, which is why it runs first.
//
//  2. Activation decides whether a present Definition is selected.
//     WhenConfigured(path) with an absent path leaves it off and reports it in
//     DisabledDetail with that reason.
//
//  3. The section decides whether a selected Definition expands to instances.
//     "plugins.<key>.enabled: false" produces no instance, and every instance
//     of a multi-instance Definition being disabled produces one entry naming
//     them.
//
// Both flags therefore apply and both are reported, while the hosted set is not
// reported as a disablement: a workload this process does not carry is a
// different fact from a plugin this process turned off, and Plan.Workloads is
// where the first is answered.
package assembly
