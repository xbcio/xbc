package placement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xbcio/xbc/plugin"
)

// start admits this placement's managed work: the renewal loop when the process
// won slots, or the standby retry loop when it won none.
//
// The renewal loop is submitted with Go rather than GoCritical on purpose. A
// renewal failure is soft placement -- the process keeps hosting what it has --
// so it must never be the reason the runtime decides the process should exit. A
// critical task that returns without being asked to would do exactly that.
//
// It is submitted here, in Start, rather than in OpenTraffic, because task
// admission is open only while a Start hook runs: a submission from
// OpenTraffic would be refused and the loop would silently never exist. The
// loop learns the traffic gate from the Context and waits on it before its
// first renewal, so it still does no externally visible work before the gate
// opens.
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
	// Whichever loop is submitted below, PreStop has one to wait for. Both of
	// them talk to the store, so both are loops the release has to be ordered
	// after: the renewal loop can write a slot back, and the standby loop can
	// win one.
	p.looping = true
	p.mu.Unlock()

	// The managed loops quit on this context rather than on their own
	// managed-task context: the managed-task context is cancelled only after
	// Stop returns, which is far too late for PreStop to be able to wait for a
	// quiet loop before it releases. This one is cancelled when the run is asked
	// to stop, which is exactly the moment the slots have to stop being renewed.
	runCtx, cancelRun := context.WithCancel(ctx)
	if standby {
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

	p.loops.Add(1)
	if !ctx.Go(func(context.Context) {
		defer p.loops.Done()
		defer cancelRun()
		defer p.markLoopDone()
		p.runRenewal(runCtx, ctx)
	}) {
		p.abandonLoop(cancelRun)
		return errors.New("placement: the runtime is not accepting the lease renewal task outside Start")
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

// runRenewal renews every held slot on a ticker until the run is asked to stop.
func (p *Placement) runRenewal(runCtx context.Context, ctx *plugin.Context) {
	if !p.awaitTrafficGate(runCtx, ctx.TrafficGate()) {
		return
	}
	ticker := time.NewTicker(p.renewEvery)
	defer ticker.Stop()
	for {
		select {
		case <-p.quiet:
			return
		case <-runCtx.Done():
			return
		case <-ticker.C:
			if !p.renewTick(runCtx) {
				return
			}
		}
	}
}

// renewTick performs one scheduled renewal round and reports whether the loop
// should keep running.
//
// Its guard is not redundant with the select that led here. select picks
// uniformly among the cases that are ready, so a tick that becomes ready in the
// same moment the run is asked to stop is chosen about half the time, and the
// round would then run against a context that is already cancelled: every store
// call fails, the renewal-failure counter records failures no store ever caused,
// and readiness reports this process as degraded while it is on its way out.
// That would make xbc_workload_lease_renew_failures_total fire on every clean
// shutdown, which is precisely the alert it is supposed to earn -- a stop is not
// a renewal failure.
//
// The guard belongs here rather than in renewHeld's error handling. "The run has
// ended" is knowable without touching the store, while suppressing a cancelled
// store call at the error site would hide the failure that matters: a real
// renewal that was cancelled or timed out on its way to a store that had stopped
// answering.
func (p *Placement) renewTick(runCtx context.Context) bool {
	if p.stopRequested(runCtx) {
		return false
	}
	p.renewHeld(runCtx)
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
func (p *Placement) renewHeld(runCtx context.Context) {
	logger := p.currentLogger()
	for _, slot := range p.snapshotHeld() {
		outcome, err := slot.renew(runCtx, p.ttl)
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

// runStandby retries acquisition until the process either wins a slot or is
// asked to stop.
//
// Winning here never means hosting: this process was assembled without the
// workload that the slot belongs to, so the slot is handed straight back and
// the process asks to be restarted, which is the only way its role can change.
// The release-then-restart window is benign competition rather than a lost
// slot -- another standby may take it, and if none does this process wins it
// again on its next start.
func (p *Placement) runStandby(runCtx context.Context, cancelRun context.CancelFunc, ctx *plugin.Context) {
	if !p.awaitTrafficGate(runCtx, ctx.TrafficGate()) {
		return
	}
	logger := ctx.Log()
	ticker := time.NewTicker(p.standbyEvery)
	defer ticker.Stop()
	for {
		select {
		case <-p.quiet:
			return
		case <-runCtx.Done():
			return
		case <-ticker.C:
		}

		// Same reason the renewal loop rechecks: a tick that is ready at the
		// same moment as the stop signal wins the select about half the time,
		// and an acquisition started from there is a process asking for
		// capacity in the middle of giving its own up.
		if p.stopRequested(runCtx) {
			return
		}
		won, _, err := p.acquireAll(runCtx, p.admittedWorkloads())
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

// preStop gives back every slot before the process's work is unwound.
//
// Retracting ownership is the whole reason this phase exists: Stop runs after
// the plugin has already lost its context, so a release attempted there would
// fail on its first remote call, and the process would hold a slot until its
// TTL expired -- holding capacity for work it is no longer doing.
//
// The order matters. The managed loop is asked to go quiet and waited for
// first, so the release is not racing a store call that is already in flight and
// so an operator reading the logs sees the loop stop before the handover. That
// applies to the standby loop as much as to the renewal loop: a renewal can
// write a slot back, and a standby can win one, so both are calls the release
// has to come after. The wait is bounded by the phase's own budget; if it
// expires, the release still runs, and the slot mutex is what actually
// guarantees a late renewal cannot write the slot back.
func (p *Placement) preStop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.quiesce()
	// Only a placement that actually started a loop has one to wait for. A
	// PreStop can run for an instance whose Start never completed -- the hook is
	// declared independently of it -- and waiting on a loop that does not exist
	// would burn the whole phase budget and then release late anyway.
	p.mu.Lock()
	looping := p.looping
	p.mu.Unlock()
	if looping {
		select {
		case <-p.loopDone:
		case <-ctx.Done():
		}
	}
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

// markLoopDone closes loopDone at most once. It is called by the managed loop
// when it returns, and by every path that never started one, so a PreStop
// waiting on it always has something to wait for.
func (p *Placement) markLoopDone() {
	p.loopDoneOnce.Do(func() { close(p.loopDone) })
}

// admittedWorkloads copies the admission order Resolve fixed, so the standby
// loop keeps attempting the same set in the same order.
func (p *Placement) admittedWorkloads() []plugin.Workload {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]plugin.Workload(nil), p.admitted...)
}
