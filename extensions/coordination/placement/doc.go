// Package placement hosts declared workloads by winning renewable slots from a
// lease store.
//
// It is the lease-backed plugin.PlacementSource: it decides, before the
// assembly graph exists, which declared workloads this process carries, and it
// then keeps that decision alive for the process's whole life.
//
// # Why the two halves live in one value
//
// The decision and the renewal describe one ownership of one slot. Splitting
// them across two values would let them disagree exactly where agreement is
// load-bearing: a decision that acquired slot 2 of sast with no renewal is a
// slot the process abandons at its TTL while still running sast's work, and a
// renewal with no decision renews a slot nobody is hosting. Keeping both in one
// *Placement is what makes "this process holds sast slot 2" a single fact.
//
// # Why the Locker arrives from outside the graph
//
// The hosted set is an input to planning, so it is settled before any
// Definition exists. A Locker supplied by a Definition would therefore arrive
// too late to decide anything, and the composition root passes one explicitly:
//
//	locker, err := redis.NewLocker(client)
//	if err != nil {
//		/* handle */
//	}
//	hosting, err := placement.New(locker, placement.WithTTL(30*time.Second))
//	if err != nil {
//		/* handle */
//	}
//	xbc.Run(
//		xbc.WithPlacement(hosting),
//		xbc.WithBundles(hosting.Bundle(), sast.Bundle()),
//	)
//
// That is the explicit cost of deciding placement before the graph; it buys the
// absence of every mechanism a mid-graph placement decision would need.
//
// # Deciding without constructing
//
// A command that needs only the decision -- doctor, or a run whose plan fails
// to build -- still wins its slots, because there is no other way to answer.
// What it does not have is the Start/PreStop/Stop lifecycle that normally gives
// them back, so Placement.Release exists for those paths: the runtime calls it
// on every way out of a command -- here, before the plugin graph exists, and at
// the end of a run behind the graph's own PreStop and Stop -- so no slot
// outlives the decision that needed it.
//
// Keeping a claim alive is part of holding it, so the renewal loop begins in
// Resolve rather than in Start: construction, migration and startup all run
// between the two, and a claim left unguarded for longer than its ttl is a claim
// a standby can take -- after which it restarts into the same role and the
// deployment runs a replica above its declared count. Release and the lifecycle
// hooks end the loop the same way: quiesce, wait for it to go quiet, then hand
// the slots back.
//
// # Soft placement
//
// A lost or unconfirmed renewal keeps the process hosting what it already
// hosts. Reserve capacity is a resource question, while abandoning live work is
// a correctness question, and mutual exclusion between replicas is guaranteed
// downstream by the queue, a distributed lock and a database compare-and-swap
// rather than by this contract. See lease's own package documentation for the
// same argument from the contract's side.
//
// The other half of that trade lives in the contract rather than here: a
// renewal that finds its own key gone re-establishes it under the same token, so
// a claim that merely lapsed -- a store outage longer than one ttl -- is
// re-confirmed on the next round instead of reading as taken over. That is what
// keeps "temporarily over the declared replica count" temporary.
//
// Readiness follows the same asymmetry rather than contradicting it: a claim
// that has gone unconfirmed for longer than one ttl is reported down, because
// by then the stored key cannot still exist and the slot in local memory may
// already be someone else's. A single unconfirmed round stays out of the probe
// and moves only the counters -- see the health check's own documentation for
// why withdrawing every holder at once is the wrong response to the store
// missing one round.
//
// # Usage
//
//	locker, err := redis.NewLocker(client)
//	if err != nil {
//		/* handle */
//	}
//	hosting, err := placement.New(locker, placement.WithTTL(30*time.Second))
//	if err != nil {
//		/* handle */
//	}
//	xbc.Run(
//		xbc.WithPlacement(hosting),
//		xbc.WithBundles(hosting.Bundle(), webprelude.Bundle(), sast.Bundle()),
//	)
//
// A process that wins no slot still starts: it hosts only the plugins that
// belong to no workload, reports ready, and retries until it wins one, at which
// point it hands the slot back and asks to be restarted so it can claim it
// during startup.
package placement
