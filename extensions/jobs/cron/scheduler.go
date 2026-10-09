package cron

import (
	"context"
	"errors"
	"fmt"
	"time"

	robfigcron "github.com/robfig/cron/v3"

	"github.com/xbcio/xbc/extensions/coordination/lease"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

type scheduledJob struct {
	job      Job
	label    string
	lockKey  string
	schedule robfigcron.Schedule
	// workload is the workload the job's contributor belongs to, empty when it
	// belongs to none. Invocations charge that workload's quota, so what the
	// scheduler runs on a workload's behalf stays inside the budget the
	// workload declared, whichever process happens to hold the job's lock.
	workload plugin.WorkloadKey
	renewals chan *leaseSession
	// runnerDone is closed when this job's runner returns, which is how its
	// renewal manager learns that no further session can arrive once drain has
	// let the last invocation finish.
	runnerDone chan struct{}
}

type leaseSession struct {
	lease       lease.Lease
	cancel      context.CancelFunc
	finished    chan struct{}
	renewalDone chan struct{}
}

// start admits every long-lived task up front. Local jobs need one runner;
// distributed jobs additionally need one renewal manager. Scheduler callbacks,
// job callbacks, and lease callbacks never submit runtime work.
func (p *Plugin) start(ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("cron: Start requires a non-nil plugin context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	p.mu.Lock()
	switch {
	case !p.initialized:
		p.mu.Unlock()
		return errors.New("cron: Start called before Init")
	case p.startAttempted:
		p.mu.Unlock()
		return errors.New("cron: Start called more than once")
	case p.stopping:
		p.mu.Unlock()
		return errors.New("cron: cannot Start after Stop")
	case p.draining:
		p.mu.Unlock()
		return errors.New("cron: cannot Start after Drain")
	}
	p.startAttempted = true
	p.starting = true
	// The plugin context follows the execution context, which XBC cancels the
	// moment shutdown begins -- before the drain phase. Deriving the job
	// contexts from it would cancel every running invocation before drain
	// could wait for it, so only its values are inherited: running jobs are
	// cancelled by Stop (runCancel) or by the end of their managed-task scope.
	runContext, runCancel := context.WithCancel(context.WithoutCancel(ctx))
	p.runCancel = runCancel
	jobs := append([]*scheduledJob(nil), p.jobs...)
	p.mu.Unlock()

	logger := ctx.Log()
	trafficGate := ctx.TrafficGate()
	if len(jobs) == 0 {
		if !p.admit(ctx, p.waitForStop) {
			return p.failStart(runCancel, "idle task", string(Key))
		}
	}
	for _, entry := range jobs {
		entry := entry
		if p.config.Distributed.Enabled {
			if !p.admit(ctx, func(taskContext context.Context) {
				p.runRenewalManager(taskContext, runContext, logger, entry)
			}) {
				return p.failStart(runCancel, "renewal manager", entry.label)
			}
		}
		if !p.admit(ctx, func(taskContext context.Context) {
			p.runJob(taskContext, runContext, trafficGate, logger, entry)
		}) {
			return p.failStart(runCancel, "job runner", entry.label)
		}
	}

	p.mu.Lock()
	if p.stopping {
		p.starting = false
		p.submissionsDone = true
		p.mu.Unlock()
		runCancel()
		p.completeTasksIfReady()
		return errors.New("cron: Stop interrupted Start")
	}
	p.starting = false
	p.started = true
	p.submissionsDone = true
	p.mu.Unlock()
	p.completeTasksIfReady()
	return nil
}

// waitForStop is the whole body of the task a plugin with no jobs submits. It
// is not a decoration: the runtime refuses to hand a process to its signal loop
// when nothing provides a long-lived capability, and a scheduler with no jobs
// is exactly such a process.
//
// It ends at drain rather than only at stop. Drain waits for the managed tasks,
// and this task exists to provide liveness rather than to hold work, so a task
// that only stop could end would make drain wait for work that was never there.
// The task's own context is the third exit, for a process whose runtime cancels
// the task scope before either phase reaches it.
func (p *Plugin) waitForStop(taskContext context.Context) {
	select {
	case <-p.drainCh:
	case <-p.stopCh:
	case <-taskContext.Done():
	}
}

// admit is called only by start while managed-task admission is open. The
// reservation precedes SubmitTask so even a host that starts and finishes the
// callback synchronously cannot race task accounting.
func (p *Plugin) admit(ctx *plugin.Context, run func(context.Context)) bool {
	p.mu.Lock()
	if p.stopping || p.submissionsDone {
		p.mu.Unlock()
		return false
	}
	p.taskCount++
	p.mu.Unlock()

	accepted := ctx.GoCritical(func(taskContext context.Context) {
		defer p.taskFinished()
		run(taskContext)
	})
	if accepted {
		return true
	}

	p.mu.Lock()
	p.taskCount--
	p.mu.Unlock()
	p.completeTasksIfReady()
	return false
}

func (p *Plugin) failStart(cancel context.CancelFunc, taskKind, label string) error {
	cancel()
	p.mu.Lock()
	p.starting = false
	p.submissionsDone = true
	p.mu.Unlock()
	p.completeTasksIfReady()
	return fmt.Errorf("cron: runtime rejected %s for %s during Start", taskKind, label)
}

func (p *Plugin) taskFinished() {
	p.mu.Lock()
	p.taskCount--
	p.mu.Unlock()
	p.completeTasksIfReady()
}

func (p *Plugin) runJob(
	taskContext context.Context,
	runContext context.Context,
	trafficGate <-chan struct{},
	logger log.Logger,
	entry *scheduledJob,
) {
	defer close(entry.runnerDone)
	ctx, cancel := mergedContext(runContext, taskContext)
	defer cancel()

	select {
	case <-trafficGate:
	case <-p.drainCh:
		return
	case <-ctx.Done():
		return
	}

	now := time.Now().In(p.location)
	next := entry.schedule.Next(now)
	if p.config.RunImmediately && !p.drainRequested() {
		p.runInvocation(ctx, logger, entry)
		if ctx.Err() != nil || p.drainRequested() {
			return
		}
	}

	for !next.IsZero() {
		if !waitUntil(ctx, p.drainCh, next) {
			return
		}
		scheduled := next
		p.runInvocation(ctx, logger, entry)
		if ctx.Err() != nil || p.drainRequested() {
			return
		}
		next = nextOccurrence(p.config.Concurrency, entry.schedule, scheduled, time.Now().In(p.location))
	}
}

func nextOccurrence(policy ConcurrencyPolicy, schedule robfigcron.Schedule, scheduled, finished time.Time) time.Time {
	if policy == ConcurrencyDelay {
		return schedule.Next(scheduled)
	}
	return schedule.Next(finished)
}

// drainRequested reports whether drain has asked the runners to stop
// scheduling, without blocking.
func (p *Plugin) drainRequested() bool {
	select {
	case <-p.drainCh:
		return true
	default:
		return false
	}
}

// waitUntil sleeps until deadline and reports whether the invocation due then
// should still run: it returns false as soon as ctx is done or drain is closed.
func waitUntil(ctx context.Context, drain <-chan struct{}, deadline time.Time) bool {
	delay := time.Until(deadline)
	if delay < 0 {
		delay = 0
	}
	timer := time.NewTimer(delay)
	defer stopTimer(timer)
	select {
	case <-timer.C:
		return true
	case <-drain:
		return false
	case <-ctx.Done():
		return false
	}
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func mergedContext(primary, secondary context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(primary)
	stopSecondary := context.AfterFunc(secondary, cancel)
	return ctx, func() {
		stopSecondary()
		cancel()
	}
}

func (p *Plugin) runInvocation(ctx context.Context, logger log.Logger, entry *scheduledJob) {
	release, admitted := p.chargeQuota(ctx, logger, entry)
	if !admitted {
		return
	}
	defer release()

	if !p.config.Distributed.Enabled {
		_ = p.invokeJob(ctx, logger, entry)
		return
	}

	// The claimant is this process, not this Plugin instance: the lock exists to
	// pick one replica out of many, so the only useful answer to "which replica
	// is running this job" is the one string that differs between them. It is
	// published beside the token and never as the token, so a superseded holder
	// cannot renew its successor's lock.
	held, acquired, err := p.locker.TryAcquire(ctx, entry.lockKey, p.process, p.config.Distributed.TTL)
	if err != nil {
		if ctx.Err() == nil {
			logger.Error("cron: distributed lease acquisition failed", "job", entry.label, "error", err)
		}
		return
	}
	if !acquired {
		logger.Debug("cron: distributed invocation skipped; lease held by another replica", "job", entry.label)
		return
	}
	if isNilInterface(held) {
		logger.Error("cron: lock backend returned an acquired nil lease", "job", entry.label)
		return
	}
	defer p.releaseLease(logger, held, entry.label)

	if ctx.Err() != nil || !p.confirmLease(ctx, logger, held, entry.label) {
		return
	}
	jobContext, jobCancel := context.WithCancel(ctx)
	session := &leaseSession{
		lease:       held,
		cancel:      jobCancel,
		finished:    make(chan struct{}),
		renewalDone: make(chan struct{}),
	}
	select {
	case entry.renewals <- session:
	case <-ctx.Done():
		jobCancel()
		return
	}

	_ = p.invokeJob(jobContext, logger, entry)
	jobCancel()
	close(session.finished)
	<-session.renewalDone
}

// chargeQuota takes one unit of the workload quota this invocation runs against
// and reports whether it may proceed. The unit is held for the whole
// invocation, including its distributed lock and lease confirmation, and given
// back when it returns.
//
// The charge is per invocation rather than around the runner because the runner
// is a scheduling loop, not work: bounding it would bound how long a process
// keeps a timer, not what the workload runs. It is taken before the distributed
// lock so a replica that is only waiting for quota does not hold the job's lock
// while it waits -- the lock picks which replica runs the job, and a replica
// that cannot run it yet has nothing to reserve.
//
// A contributor that belongs to no workload charges nothing, which is what the
// runtime reports for a plugin outside every workload.
func (p *Plugin) chargeQuota(ctx context.Context, logger log.Logger, entry *scheduledJob) (func(), bool) {
	admission := p.admissions[entry.workload]
	if admission == nil {
		return func() {}, true
	}
	release, err := admission.Acquire(ctx)
	if err != nil {
		if ctx.Err() == nil {
			logger.Error("cron: invocation could not take its workload's quota", "job", entry.label, "error", err.Error())
		}
		return nil, false
	}
	return release, true
}

func (p *Plugin) runRenewalManager(
	taskContext context.Context,
	runContext context.Context,
	logger log.Logger,
	entry *scheduledJob,
) {
	ctx, cancel := mergedContext(runContext, taskContext)
	defer cancel()
	for {
		select {
		case session := <-entry.renewals:
			p.renewSession(ctx, logger, entry.label, session)
		case <-entry.runnerDone:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (p *Plugin) renewSession(ctx context.Context, logger log.Logger, label string, session *leaseSession) {
	defer close(session.renewalDone)
	ticker := time.NewTicker(p.renewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-session.finished:
			return
		case <-ctx.Done():
			session.cancel()
			return
		case <-ticker.C:
			renewContext, cancel := context.WithTimeout(ctx, p.renewTimeout())
			owned, err := session.lease.Renew(renewContext, p.config.Distributed.TTL)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					logger.Error("cron: lease renewal failed; cancelling job", "job", label, "error", err)
				}
				session.cancel()
				return
			}
			if !owned {
				logger.Error("cron: lease ownership lost; cancelling job", "job", label)
				session.cancel()
				return
			}
		}
	}
}

func (p *Plugin) confirmLease(ctx context.Context, logger log.Logger, held lease.Lease, label string) bool {
	confirmContext, cancel := context.WithTimeout(ctx, p.renewTimeout())
	owned, err := held.Renew(confirmContext, p.config.Distributed.TTL)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			logger.Error("cron: acquired lease could not be confirmed; skipping job", "job", label, "error", err)
		}
		return false
	}
	if !owned {
		logger.Debug("cron: acquired lease expired before execution; skipping job", "job", label)
		return false
	}
	return true
}

func (p *Plugin) renewTimeout() time.Duration {
	timeout := p.renewInterval
	if third := p.config.Distributed.TTL / 3; third < timeout {
		timeout = third
	}
	return timeout
}

func (p *Plugin) releaseLease(logger log.Logger, held lease.Lease, label string) {
	timeout := p.config.Distributed.TTL / 2
	if timeout <= 0 || timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err := held.Release(ctx); err != nil {
		logger.Error("cron: lease release failed", "job", label, "error", err)
	}
}
