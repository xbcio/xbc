package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/xbcio/xbc/log"
)

// osExit is the process-termination hook runProcess and exitWithError go
// through -- indirected through a package var, mirroring log/zap.go's own
// exitFunc, so same-package tests can observe the process tail in-process
// without forking a subprocess.
var osExit = os.Exit

// stdout is the process stream read-only commands write to. Like osExit it is
// owned here rather than by the command itself, so that no other file in the
// package needs to reach for the process.
var stdout io.Writer = os.Stdout

// stderr is the process stream failures and the forced-termination notice go
// to. Owned here for the same reason as stdout.
var stderr io.Writer = os.Stderr

// runProcess runs an assembled App as the current process. It is the sole
// owner of process arguments, signal registration, diagnostics, logger
// flushing, and process termination.
func runProcess(app *App) {
	exitApp(app, os.Args[1:])
}

// exitWithError handles a failure that occurred before an App could be built.
// Logging may not be configured yet, so the error is written directly to
// stderr before the global logger is flushed and the process exits with 1.
func exitWithError(err error) {
	fmt.Fprintln(stderr, err)
	_ = log.Sync()
	osExit(1)
}

// forceQuit abandons a shutdown the operator has stopped waiting for. It is
// the only path that can end a process whose plugin hook never returns:
// requesting a stop cancels contexts and closes the stop channel, but it
// cannot interrupt an Init, Migrate, Start, or OpenTraffic that blocks
// forever, so App.execute never returns and the ordinary exit tail is never
// reached. Without this escalation the operator's only remaining tool is
// SIGKILL.
//
// The notice is not optional: a process that disappears on the second Ctrl-C
// without saying why is its own operational mystery. The tail then mirrors
// exitApp and exitWithError -- report, flush the logger, terminate -- and uses
// exit code 1, because a shutdown that was abandoned did not complete.
//
// It is a package var, like osExit, so a same-package test can drive the
// watcher's escalation path with process termination faked out.
var forceQuit = func() {
	fmt.Fprintln(stderr, "xbc: repeated stop signal, abandoning graceful shutdown and terminating now")
	_ = log.Sync()
	osExit(1)
}

// executeWithSignals is the process adapter used by runProcess. Keeping it
// separate from App.Execute makes the ownership boundary explicit and gives
// tests a way to exercise real signal delivery without allowing os.Exit to
// terminate the test process.
func executeWithSignals(app *App, args []string) (int, error) {
	stopSignals := watchProcessSignals(
		func() { app.requestStop(stopReasonSignal) },
		func() { forceQuit() },
	)
	defer stopSignals()
	return app.execute(context.Background(), args, stopReasonSignal)
}

// watchProcessSignals is the only OS-signal subscription in the framework.
// It belongs to the process adapter, never to App.Execute.
//
// The first stop signal calls onStopRequest, which asks for a graceful
// shutdown; every later one calls onForceQuit. Escalation counts the signals
// this watcher has received rather than asking whether the stop request was
// the one that won: an application already shutting down for another reason --
// a failed critical task, a canceled parent context -- would otherwise make
// the operator's very first Ctrl-C look like a repeat and kill a shutdown that
// was still making progress.
func watchProcessSignals(onStopRequest, onForceQuit func()) (stop func()) {
	// Keep room for both supported signals while the first callback is being
	// handled. os/signal deliberately uses non-blocking sends, so an
	// unbuffered (or permanently unread) channel could silently lose the
	// operator's second request.
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)

	done := make(chan struct{})
	var (
		stopOnce sync.Once
		watchers sync.WaitGroup
	)
	watchers.Add(1)
	go func() {
		defer watchers.Done()
		// received is confined to this goroutine, so the escalation decision
		// needs no synchronization of its own.
		received := 0
		for {
			select {
			case <-ch:
				received++
				if received == 1 {
					onStopRequest()
					continue
				}
				onForceQuit()
			case <-done:
				return
			}
		}
	}()

	return func() {
		stopOnce.Do(func() {
			signal.Stop(ch)
			close(done)
			watchers.Wait()
		})
	}
}

// exitApp owns the process-only tail: OS signal registration, diagnostics,
// logger flushing, and process termination. App.Execute does none of these.
func exitApp(app *App, args []string) {
	code, err := executeWithSignals(app, args)
	if err != nil {
		fmt.Fprintln(stderr, err)
	}
	_ = log.Sync()
	osExit(code)
}
