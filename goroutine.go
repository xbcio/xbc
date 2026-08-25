package xbc

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"

	"github.com/xbcio/xbc/log"
)

// initGoroutines wires up the App-level machinery Context.Go and
// Context.GoCritical depend on: a WaitGroup the shutdown path waits on, a
// cancellable root context every managed goroutine receives, and a signal
// channel stage 9's serve() (stage_run.go) selects on to learn a
// GoCritical failure happened.
//
// cli.go calls this once, before stage 5 (Init) runs -- a plugin's Init
// already receives a live *Context and is free to call ctx.Go from inside
// Init itself, so this machinery must exist before initAll runs, not just
// before startRunners.
func (a *App) initGoroutines() {
	a.wg = &sync.WaitGroup{}
	a.runCtx, a.cancel = context.WithCancel(context.Background())
	a.criticalCh = make(chan struct{})
}

// goManaged is the single engine behind Context.Go and Context.GoCritical
// (their method bodies live in context.go and just forward here -- see
// context.go's Go/GoCritical). The two are the same mechanism -- spawn,
// recover, wait -- differing only in what happens after a panic or an
// unrequested return, so splitting them into separate implementations would
// just be the same code twice with one branch flipped.
//
// Go: a panic inside fn is recovered and logged; the goroutine simply
// ends. A normal return is treated as "the task finished", nothing more --
// the right default for one-shot work like a cache refresh or a metrics
// push, where "it stopped" is not itself bad news.
//
// GoCritical: for goroutines whose death means the application is no
// longer doing its job even though the process is still up -- a Kafka
// consumer, a long-lived gateway connection, a data sync loop. Both a
// panic and an unprompted normal return are treated as the application
// having failed, and trigger a full stage-10 shutdown (not os.Exit):
// in-flight HTTP requests still drain, every other plugin still gets
// Stop() in reverse topological order, and the process exits with code 1
// so the surrounding orchestrator knows this was not a clean stop.
func (a *App) goManaged(ctx *Context, fn func(context.Context), critical bool) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()

		// expected records whether the eventual return (if fn doesn't
		// panic) happened because shutdown was already underway. It is
		// only read by the deferred recover below, in the same goroutine,
		// after fn has returned -- no concurrent access, no race.
		expected := false

		defer func() {
			if r := recover(); r != nil {
				ctx.Log().Error("xbc: 托管 goroutine panic，已恢复",
					"panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
				if critical {
					a.triggerCritical(fmt.Sprintf("插件 %s 的托管 goroutine panic: %v", ctx.Name(), r))
				}
				return
			}
			if critical && !expected {
				a.triggerCritical(fmt.Sprintf("插件 %s 的托管 goroutine 意外提前返回", ctx.Name()))
			}
		}()

		fn(a.runCtx)
		// Reaching here means fn returned without panicking. If runCtx is
		// already cancelled, this return was requested (shutdown is
		// already in flight) and is therefore expected, whatever the
		// reason for the cancellation was. If it isn't cancelled yet,
		// nobody asked this goroutine to stop -- for GoCritical that is
		// exactly the "consumer silently stopped consuming" failure this
		// whole mechanism exists to catch.
		expected = a.runCtx.Err() != nil
	}()
}

// triggerCritical fires the application-wide shutdown alarm. It runs from
// arbitrary, possibly concurrent goroutines, so firing it must itself be
// race-safe and must never leave a caller stuck: criticalOnce guarantees
// exactly one caller does the actual work (recording the reason, logging
// it, closing criticalCh), while every other concurrent caller's Do call
// returns as soon as that tiny critical section finishes -- closing a
// channel twice panics, so collapsing concurrent triggers into one is not
// optional, it is the only thing standing between a double panic-in-a-
// panic and a clean shutdown.
//
// "Non-blocking" here does not mean "channel send with a buffer of 1" --
// closing a channel never blocks regardless of buffering. What it actually
// guards against is a goroutine getting wedged forever waiting on another
// goroutine that will never look at what it sent. Once's guarded function
// always runs to completion and returns quickly and unconditionally, so no
// caller of triggerCritical can get stuck on it.
func (a *App) triggerCritical(reason string) {
	a.criticalOnce.Do(func() {
		a.criticalReason = reason
		log.L().Error("xbc: 收到 critical 信号，即将触发应用关闭", "reason", reason)
		close(a.criticalCh)
	})
	if a.cancel != nil {
		a.cancel()
	}
}
