package placement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xbcio/xbc/plugin"
)

// start admits this placement's managed work: nothing when the process won
// slots, or the standby retry loop when it won none.
//
// The renewal loop is not submitted here. It exists from the moment the
// decision does -- the round that won a slot started it, in Resolve -- because
// keeping a claim alive is part of holding it, and a keepalive that waited for
// Start left the ttl unguarded for the whole of construction and migration. See
// startRenewalLoop.
//
// The standby loop still begins in Start because it needs the plugin Context:
// winning a slot makes the process ask to be restarted, and RequestShutdown is
// the Context's. It is submitted with Go rather than GoCritical on purpose: a
// standby that keeps losing has nothing to report, and neither loop may ever be
// the reason the runtime decides the process should exit.
func (p *Placement) start(ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("placement: Start requires a non-nil plugin context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	p.mu.Lock()
	switch {
	case p.stopping:
		p.mu.Unlock()
		return errors.New("placement: Start called after Stop")
	case p.started:
		p.mu.Unlock()
		return errors.New("placement: Start called more than once")
	case !p.resolved:
		p.mu.Unlock()
		// A second New in the same process replaces the first one everywhere the
		// package-wide Definition can see, so an application that composed the
		// first Placement correctly now holds a Definition pointing at a
		// Placement that never resolved. That is a different mistake from
		// selecting the Bundle without building a Placement at all, and it is
		// worth naming, because the composition in front of the operator looks
		// right.
		if current := installedPlacement.Load(); current != nil && current != p {
			return errors.New("placement: another placement.New call replaced this process's placement; one process decides one hosted set, so build one Placement and pass that one to both WithPlacement and its Bundle")
		}
		return errors.New("placement: Start requires a decided placement; select this Bundle together with the Placement that resolved it")
	}
	p.started = true
	p.logger = ctx.Log()
	held := append([]*heldSlot(nil), p.held...)
	standby := len(held) == 0
	if standby {
		// The standby loop is this placement's only loop, and PreStop has to
		// have one to wait for. It talks to the store -- it can win a slot --
		// so it is a loop the release has to be ordered after.
		p.looping = true
	}
	p.mu.Unlock()

	if !standby {
		return nil
	}

	// The standby loop quits on this context rather than on its own
	// managed-task context: the managed-task context is cancelled only after
	// Stop returns, which is far too late for PreStop to be able to wait for a
	// quiet loop before it releases. This one is cancelled when the run is asked
	// to stop, which is exactly the moment the loop has to stop asking for
	// capacity.
	runCtx, cancelRun := context.WithCancel(ctx)
	p.loops.Add(1)
	if !ctx.Go(func(context.Context) {
		defer p.loops.Done()
		defer cancelRun()
		defer p.markLoopDone()
		p.runStandby(runCtx, cancelRun, ctx)
	}) {
		p.abandonLoop(cancelRun)
		return errors.New("placement: the runtime is not accepting the standby retry task outside Start")
	}
	return nil
}

// abandonLoop puts back everything start owed a loop the runtime refused.
//
// All three are state a goroutine would have cleaned up on its way out, and a
// refused submission means no goroutine ever runs. The wait-group count matters
// most: stop waits on that group, so a count nothing will ever decrement wedges
// every later shutdown until its budget expires. The done signal is next: a
// PreStop waiting for a loop that does not exist burns the whole phase budget
// and then releases late anyway. The cancel function leaks the least -- a child
// context stays attached to the plugin's context until the run ends -- but it is
// the same omission, and the run can be very long.
func (p *Placement) abandonLoop(cancelRun context.CancelFunc) {
	p.loops.Done()
	p.markLoopDone()
	cancelRun()
}

// runRenewal renews every held slot on a ticker until the placement is
// quiesced.
//
// The loop begins with the decision rather than with Start (startRenewalLoop
// says why) and its first round runs at once rather than after a tick, because
// the claim it keeps alive already exists by the time it is started.
//
// It does not wait for the traffic gate. Renewal is not traffic: it publishes
// nothing to users, serves no request, and is invisible until the process fails
// to do it, so the gate that holds back externally visible work has nothing to
// hold back here. Ownership starts when the slot is won, and waiting for the
// gate is what left a slow startup's claim unguarded long enough to expire and
// be handed to a standby -- which then restarted into the same role, putting a
// second copy of the workload in front of users.
//
// The store calls run on a context the loop owns and nothing cancels, so a
// round that is already in flight when the placement quiesces finishes instead
// of failing: a stop is not a renewal failure, and a clean shutdown must not
// make xbc_workload_lease_renew_failures_total fire. The loop still exits
// promptly, between rounds, on the quiet signal. Bounding the wait for a round
// in flight is not this loop's job: the Locker contract requires implementations
// to bound their own calls, and Resolve already reads the store on the same
// terms.
func (p *Placement) runRenewal() {
	if !p.renewTick() {
		return
	}
	ticker := time.NewTicker(p.renewEvery)
	defer ticker.Stop()
	for {
		select {
		case <-p.quiet:
			return
		case <-ticker.C:
			if !p.renewTick() {
				return
			}
		}
	}
}

// renewTick performs one scheduled renewal round and reports whether the loop
// should keep running.
//
// The guard is not redundant with the select that led here. select picks
// uniformly among the cases that are ready, so a tick that becomes ready in the
// same moment the placement is quiesced is chosen about half the time, and the
// round would run after the signal to stop renewing. The slot mutex keeps such a
// round from writing anything back after a release, but it is still store
// traffic nobody can act on: everything downstream is already waiting for this
// loop to go quiet.
func (p *Placement) renewTick() bool {
	select {
	case <-p.quiet:
		return false
	default:
	}
	p.renewHeld(context.Background())
	return true
}

// renewHeld performs one renewal round over every held slot.
//
// A failed or unconfirmed renewal is recorded and the process keeps hosting.
// That is the design's central trade: the worst outcome of a lapsed lease is
// one workload running a replica or two above its declared count, which is a
// resource problem, while the alternative -- giving the slot up -- is a
// cluster-wide reshuffle triggered by a blip in a store whose availability the
// contract deliberately does not promise.
//
// The context bounds one store round, and the renewal loop passes a background
// one on purpose: a round cut short by this placement's own shutdown would be
// recorded as a renewal failure the store never caused.
func (p *Placement) renewHeld(ctx context.Context) {
	logger := p.currentLogger()
	for _, slot := range p.snapshotHeld() {
		outcome, err := slot.renew(ctx, p.ttl)
		switch {
		case err != nil:
			p.renewFailures.Add(1)
			logger.Warn("placement: renewing a workload slot failed, this process keeps hosting it",
				"workload", slot.workload.String(), "slot", slot.index, "key", slot.key, "error", err)
		case outcome == renewLost:
			p.renewFailures.Add(1)
			logger.Warn("placement: a workload slot is no longer confirmed as owned by this process, this process keeps hosting it",
				"workload", slot.workload.String(), "slot", slot.index, "key", slot.key)
		}
	}
}

// standbyWait is how long this standby waits before its next attempt: the
// configured interval with up to half of it added or subtracted.
//
// The jitter is what keeps a fleet of standbys from retrying in lockstep. A
// fixed period has no memory of which processes are asking together, so
// processes that started together -- a rolling restart, a node whose pods all
// came back at once -- keep their phase for the life of the run, and the round
// that finds a freed slot finds it for all of them at once. The mean wait stays
// the configured interval, so the store sees the same load; the cost is that
// the worst case is half an interval longer, which is the half the arithmetic
// in the deployment documentation accounts for.
func (p *Placement) standbyWait() time.Duration {
	return p.standbyEvery + p.randomJitter(p.standbyEvery/2)
}

// runStandby retries acquisition until the process either wins a slot or is
// asked to stop.
//
// Winning here never means hosting: this process was assembled without the
// workload that the slot belongs to, so the slot is handed straight back and
// the process asks to be restarted, which is the only way its role can change.
// The release-then-restart window is benign competition rather than a lost
// slot -- another standby may take it, and if none does this process wins it
// again on its next start.
//
// The retry period is jittered per round (standbyWait) because the round that
// wins is the round that restarts the process: standbys polling in lockstep
// win, release and restart together, and a fleet that restarts together is the
// state the jitter exists to leave.
func (p *Placement) runStandby(runCtx context.Context, cancelRun context.CancelFunc, ctx *plugin.Context) {
	if !p.awaitTrafficGate(runCtx, ctx.TrafficGate()) {
		return
	}
	logger := ctx.Log()
	for {
		timer := time.NewTimer(p.standbyWait())
		select {
		case <-p.quiet:
			timer.Stop()
			return
		case <-runCtx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		// Same reason the renewal loop rechecks: a tick that is ready at the
		// same moment as the stop signal wins the select about half the time,
		// and an acquisition started from there is a process asking for
		// capacity in the middle of giving its own up.
		if p.stopRequested(runCtx) {
			return
		}
		admitted, instance := p.admission()
		won, _, err := p.acquireAll(runCtx, admitted, instance)
		if err != nil {
			// A round that failed part-way still won the slots that came before
			// the failure, and this is the call site that can record them:
			// unlike Resolve it holds no lock, so adoptHandback is safe here
			// (acquireAll explains why the same call cannot live inside it).
			// Without this the slot is referenced nowhere -- not held, not
			// owed -- and a standby retrying a flaky store would strand one
			// slot per failed round until each expired with its ttl.
			//
			// The release goes through handBack for the same reason the win
			// below does: runCtx may already be cancelled, and a release that
			// inherited it would fail on every call while reporting only a log
			// line.
			if owed, releaseErr := p.handBack(runCtx, won); len(owed) > 0 {
				p.adoptHandback(owed)
				logger.Warn("placement: standby could not give back a slot it won before the round failed, the release is retried on stop", "error", releaseErr)
			}
			// A standby that cannot reach the store has nothing to report
			// upward: it already hosts its unowned plugins and stays ready,
			// which is the whole point of the standby shape. It keeps trying,
			// so a store that comes back is picked up without a restart.
			logger.Warn("placement: standby could not reach the lease store, retrying", "error", err)
			continue
		}
		if len(won) == 0 {
			continue
		}
		keys := make([]string, 0, len(won))
		for _, slot := range won {
			keys = append(keys, slot.key)
		}
		// The recheck above narrows the shutdown window but cannot close it: the
		// run can be cancelled while this acquisition is in flight, and a store
		// that consults the caller's context only before its round trip -- which
		// is most of them -- grants the slot anyway. From here on the slot is
		// this process's to give back even though it never hosted it, so it is
		// recorded before the handover is attempted: that is what leaves the Stop
		// backstop something to retry if the handover does not complete, and
		// without it a slot won in that window is referenced nowhere and can only
		// expire with its ttl.
		p.adoptHandback(won)
		if _, releaseErr := p.handBack(runCtx, won); releaseErr != nil {
			logger.Warn("placement: standby could not hand back a slot it won, the release is retried on stop", "error", releaseErr)
		}
		logger.Info("placement: standby won a slot and is restarting to take it up", "slots", keys)
		cancelRun()
		ctx.RequestShutdown("placement: won " + fmt.Sprint(keys) + " as a standby, restarting to host it")
		return
	}
}

// handbackBudget bounds a release taken on a context whose own cancellation had
// to be dropped, so something has to bound it in that context's place.
//
// It is a constant rather than a knob because nothing about it is a deployment
// decision: it is long enough for a store that is answering to answer, and short
// enough that a store that is not cannot hold a stopping process open. A slot
// that cannot be handed back inside it expires with its ttl, which is the outcome
// this whole path exists to avoid paying for, so the bound is deliberately
// generous relative to one round trip.
const handbackBudget = 2 * time.Second

// handBack gives slots back on a context that does not inherit the caller's
// cancellation, and reports the slots the store did not confirm.
//
// This is §4.10's argument for PreStop applied where it is easiest to miss. The
// run context is cancelled the moment the process is asked to stop, and a
// release that inherits it fails on its first store call -- while the only
// report of that failure is a log line, so a handover that never happens looks
// exactly like one that did. Every caller here is on such a path: the standby
// loop hands back on the very run context the stop request cancels. The caller's
// values are kept, because they identify it to whatever the store is; only its
// cancellation is dropped, and a budget replaces it so a store that never answers
// cannot keep the loop alive.
//
// Returning the slots that are still owed, rather than only an error, is what
// lets a caller put them on the backstop's list: the error says a handover
// failed, the slice says which claims a later attempt has to make again. Release
// is idempotent, so a slot the store did confirm is simply absent from it and
// costs the backstop nothing.
func (p *Placement) handBack(callerCtx context.Context, slots []*heldSlot) ([]*heldSlot, error) {
	if len(slots) == 0 {
		return nil, nil
	}
	releaseCtx, cancelRelease := context.WithTimeout(context.WithoutCancel(callerCtx), handbackBudget)
	defer cancelRelease()
	var owed []*heldSlot
	var failures []error
	for _, slot := range slots {
		if err := slot.release(releaseCtx); err != nil {
			owed = append(owed, slot)
			failures = append(failures, err)
		}
	}
	return owed, errors.Join(failures...)
}

// awaitTrafficGate blocks until the route gate opens. It reports false when the
// run ended first, so a loop that never got to do any work exits rather than
// proceeding past a closed gate.
func (p *Placement) awaitTrafficGate(runCtx context.Context, gate <-chan struct{}) bool {
	select {
	case <-gate:
		return true
	case <-p.quiet:
		return false
	case <-runCtx.Done():
		return false
	}
}

// quiesce makes both managed loops stop touching the store, and is one-way: a
// placement on its way out never resumes.
func (p *Placement) quiesce() {
	p.mu.Lock()
	p.stopping = true
	p.mu.Unlock()
	p.quietOnce.Do(func() { close(p.quiet) })
}

// awaitLoopQuiet waits for this placement's loop to stop touching the store, or
// for ctx to run out.
//
// Every release is ordered after this wait, because a loop that is still running
// can write a slot back: the renewal loop can extend a claim the release is
// taking back, and the standby loop can win one. The wait is bounded by the
// caller's context -- a phase budget for PreStop, the caller's own for Release
// -- and it is deliberately not an error to run out: the release still runs
// afterwards, and the slot mutex is what actually guarantees a late renewal
// cannot write a released slot back.
//
// Only a placement that actually started a loop has one to wait for. A release
// can run for an instance whose Start never completed -- PreStop is declared
// independently of it, and Release covers every path between the decision and a
// constructed plugin -- and waiting on a loop that does not exist would burn the
// whole budget and then release late anyway.
func (p *Placement) awaitLoopQuiet(ctx context.Context) {
	p.mu.Lock()
	looping := p.looping
	p.mu.Unlock()
	if !looping {
		return
	}
	select {
	case <-p.loopDone:
	case <-ctx.Done():
	}
}

// preStop gives back every slot before the process's work is unwound.
//
// Retracting ownership is the whole reason this phase exists: Stop runs after
// the plugin has already lost its context, so a release attempted there would
// fail on its first remote call, and the process would hold a slot until its
// TTL expired -- holding capacity for work it is no longer doing.
//
// The order matters. The loops are asked to go quiet and waited for first, so
// the release is not racing a store call that is already in flight and so an
// operator reading the logs sees the loops stop before the handover. That
// applies to the standby loop as much as to the renewal loop: a renewal can
// write a slot back, and a standby can win one, so both are calls the release
// has to come after. See awaitLoopQuiet for why running out of budget is not an
// error.
func (p *Placement) preStop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.quiesce()
	p.awaitLoopQuiet(ctx)
	return releaseAll(ctx, p.releasable())
}

// stop is the backstop release.
//
// PreStop is the designed release point and runs first in every ordinary
// shutdown. When the pre-stop phase is configured away -- xbc.pre_stop_timeout
// is 0s, so no hook is called at all -- this is what keeps a normal stop from
// leaving the slots to expire on their own, and it is idempotent so the
// ordinary path pays nothing for it.
func (p *Placement) stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.quiesce()
	p.markLoopDone()
	p.loops.Wait()
	return releaseAll(ctx, p.releasable())
}

// markLoopDone closes loopDone at most once. It is called by a loop when it
// returns, and by every path that will never start one -- a refused submission,
// a stop that raced the decision, the Stop backstop -- so a PreStop or a Release
// waiting on it always has something to wait for.
func (p *Placement) markLoopDone() {
	p.loopDoneOnce.Do(func() { close(p.loopDone) })
}

// admission copies what Resolve fixed for the rounds that come after it: the
// admission order, so the standby loop keeps attempting the same set in the same
// order, and the claimant to publish, so a slot won on a retry names the same
// process the first round would have. Both are written once by the resolving
// round under mu, and read together here so a retry can never pair one round's
// workloads with another's identity.
func (p *Placement) admission() ([]plugin.Workload, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]plugin.Workload(nil), p.admitted...), p.instance
}
