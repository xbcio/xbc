package runtime

import (
	"context"
	"fmt"
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
	fmt.Fprintln(os.Stderr, err)
	_ = log.Sync()
	osExit(1)
}

// executeWithSignals is the process adapter used by runProcess. Keeping it
// separate from App.Execute makes the ownership boundary explicit and gives
// tests a way to exercise real signal delivery without allowing os.Exit to
// terminate the test process.
func executeWithSignals(app *App, args []string) (int, error) {
	stopSignals := watchProcessSignals(func() { app.requestStop(stopReasonSignal) })
	defer stopSignals()
	return app.execute(context.Background(), args, stopReasonSignal)
}

// watchProcessSignals is the only OS-signal subscription in the framework.
// It belongs to the process adapter, never to App.Execute.
func watchProcessSignals(onSignal func()) (stop func()) {
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
		for {
			select {
			case <-ch:
				onSignal()
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
		fmt.Fprintln(os.Stderr, err)
	}
	_ = log.Sync()
	osExit(code)
}
