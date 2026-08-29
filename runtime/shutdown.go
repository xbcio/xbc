package runtime

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/xbcio/xbc/assembly"
	"github.com/xbcio/xbc/plugin"
)

// stopReasonStartupFailed labels cancellation and unwind triggered by a
// startup stage returning an error, as opposed to an external stop request.
const stopReasonStartupFailed = "startup-failed"

// abort unwinds a partially started application and hands back the error
// that caused it, unchanged.
//
// Returning cause rather than the unwind's own error is deliberate: the
// original failure is the one the operator has to act on, and letting a
// secondary Stop failure overwrite it would hide the real cause behind a
// symptom of it. Unwind failures are still surfaced -- through the log, at
// error level, naming the plugin -- just not in the position reserved for
// the root cause.
func (a *App) abort(cause error) error {
	if err := a.unwind(stopReasonStartupFailed); err != nil {
		a.log().Error("xbc: cleanup after failed startup was not fully successful", "error", err)
	}
	return cause
}

// unwind first cancels every execution-scoped plugin Context, then stops the
// application exactly once, however many callers ask. requestStop preserves
// the first reason, so a startup error discovered after a signal/context/
// critical request cannot relabel the shutdown as startup-failed. Conversely,
// an ordinary startup failure or successful one-shot completion now closes
// Context.Done before the first Stop begins instead of leaving the execution
// scope live after its resources have started unwinding.
//
// Idempotence is a data property here, not a call-site convention. Both the
// startup-failure path and the run loop lead here, and a signal can arrive
// while a critical-triggered unwind is already in flight. A Closer such as a
// database pool cannot survive being Stopped twice, so the guarantee has to
// hold no matter how the paths interleave -- which sync.Once gives and
// careful ordering of call sites does not.
func (a *App) unwind(reason string) error {
	// This must precede unwindOnce.Do: even a caller that loses a concurrent
	// unwind race must publish its stop request before waiting for the winning
	// unwind, while sync.Once on requestStop keeps the earliest reason stable.
	a.requestStop(reason)

	// Use the winning reason rather than this caller's candidate. In particular,
	// abort(startup-failed) commonly follows a context/signal cancellation that
	// already closed stopCh, and must not change the reason reported by unwind.
	a.unwindOnce.Do(func() { a.unwindErr = a.doUnwind(a.stopReason) })
	return a.unwindErr
}

// doUnwind is the actual shutdown: close task admission, then walk every
// initialized plugin in reverse dependency order, stopping each one and
// retiring its managed tasks before moving to the next -- all inside one
// shared budget.
//
// # One budget for the whole unwind
//
// Not one per plugin. A per-plugin budget reads as fairer but makes the worst
// case scale with the number of plugins: twenty plugins with a 30s budget each
// is a ten-minute shutdown, and the orchestrator's own kill timer -- which is
// what xbc.shutdown_timeout exists to stay inside of -- has no idea how many
// plugins there are. A shared deadline means the promise "this process stops
// within xbc.shutdown_timeout" holds regardless of how the application is
// assembled. The cost is that one plugin can consume the budget the others
// needed; that cost is paid visibly, as an error naming that plugin, rather
// than silently as a process that overran and got SIGKILLed.
//
// # Why Stop comes before that plugin's tasks are cancelled
//
// This is the part that is easy to get backwards, and getting it backwards
// breaks graceful shutdown outright.
//
// The intuition "tasks use the plugin's resources, so retire the tasks first"
// is right about the hazard and wrong about the remedy. A graceful Stop is
// usually implemented *through* the plugin's own managed tasks: an HTTP
// server drains in-flight requests, and those requests are being served by its
// managed serving goroutine. Cancel that goroutine first and there is nothing
// left to drain through -- Stop either returns instantly having dropped live
// connections, or blocks waiting for a drain that can no longer make progress.
//
// So the order is per plugin, interleaved: Stop runs while that plugin's tasks
// are still alive and able to finish their work, and only once Stop has
// returned is its task context cancelled and its tasks waited for. The
// original hazard is still handled, because the walk is in reverse dependency
// order: by the time a plugin is stopped, every plugin that depends on it has
// already been stopped and had its tasks retired, so nothing is left using the
// resources it is about to release.
//
// # When the budget runs out
//
// Every remaining plugin is going to be reported as timed out regardless, so
// the moment the deadline expires all outstanding task contexts are cancelled
// at once. The remaining Stops are still attempted -- a Stop that returns
// promptly still releases its resources, and skipping it would guarantee a
// leak to save nothing -- but nothing is waited for any more.
func (a *App) doUnwind(reason string) error {
	a.log().Info("xbc: starting shutdown", "reason", reason)

	deadline, cancel := context.WithTimeout(context.Background(), a.settings.ShutdownTimeout)
	defer cancel()

	// First, before any Stop runs. This both closes the admission gate that
	// makes every later wait race-free, and raises the shutdown flag that
	// tells a critical task returning from inside its own plugin's Stop that
	// its return was asked for rather than a failure.
	a.tasks.closeAdmission()

	var errs []error
	initialized := a.container.Initialized()
	for i := len(initialized) - 1; i >= 0; i-- {
		inst := initialized[i]

		if closer, ok := inst.Plugin().(plugin.Closer); ok {
			if err := stopBounded(deadline, a.settings.ShutdownTimeout, inst, closer); err != nil {
				a.log().Error("xbc: plugin stop failed, continuing to shut down other plugins",
					"plugin", inst.Label(), "error", err)
				errs = append(errs, err)
			}
		}

		if err := a.tasks.stopPlugin(inst.Identity(), deadline); err != nil {
			a.log().Error("xbc: plugin's managed task did not exit in time, continuing to shut down other plugins",
				"plugin", inst.Label(), "error", err)
			errs = append(errs, err)
		}

		if deadline.Err() != nil {
			a.tasks.cancelAll()
		}
	}

	errs = append(errs, a.tasks.drainRemaining(deadline)...)
	return errors.Join(errs...)
}

// stopBounded calls one plugin's Stop and returns within the shared budget no
// matter what that Stop does.
//
// Three things can go wrong, and a plain `return closer.Stop(ctx)` survives
// none of them:
//
//   - Stop returns an error. Wrapped with the plugin's identity, since the
//     error a Closer returns rarely says which plugin it came from.
//   - Stop panics. Recovered here, because a panic escaping the shutdown path
//     would take down the process before any remaining plugin got its Stop.
//   - Stop never returns. This is the case the goroutine exists for. A Stop
//     that blocks forever -- waiting on a connection that will never drain,
//     on a lock nobody releases -- cannot be interrupted from outside; Go has
//     no way to kill a goroutine. Handing it a context it may or may not
//     honor is not a bound, it is a request. The only real bound is to stop
//     waiting: the goroutine is abandoned, deliberately leaked, and the
//     process continues stopping everything else and then exits, which
//     collects the leak.
//
// The send on done never blocks even after this function has stopped
// listening: the channel is buffered, so an abandoned Stop that eventually
// finishes does not add a second, permanent leak on top of the first.
func stopBounded(deadline context.Context, budget time.Duration, inst *assembly.Instance, closer plugin.Closer) error {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("xbc: plugin %s's Stop panic: %v\n%s",
					inst.Label(), r, debug.Stack())
			}
		}()
		done <- closer.Stop(deadline)
	}()

	select {
	case err := <-done:
		return wrapStopError(inst, err)
	case <-deadline.Done():
	}

	// The budget is gone. Look once more before declaring a timeout: Stop may
	// have returned in the same instant the deadline expired, and reporting a
	// plugin that did stop as one that hung would be a claim the operator has
	// no way to check and would waste their time chasing.
	select {
	case err := <-done:
		return wrapStopError(inst, err)
	default:
		return fmt.Errorf(
			"xbc: plugin %s's Stop did not return within the shutdown budget (%s), giving up and continuing to shut down other plugins\n"+
				"  → this goroutine is intentionally leaked and will be reclaimed when the process exits; check if Stop is waiting for an operation that will never complete",
			inst.Label(), budget)
	}
}

// wrapStopError attaches the plugin's identity to whatever Stop returned. A
// nil error stays nil -- fmt.Errorf on a nil %w would manufacture a failure
// out of a success.
func wrapStopError(inst *assembly.Instance, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("xbc: plugin %s stop failed: %w", inst.Label(), err)
}
