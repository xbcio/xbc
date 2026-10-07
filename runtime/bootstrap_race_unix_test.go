//go:build unix && !aix && !solaris

package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stopDuringBootstrapRounds is how many times the race window below is
// replayed. Detection depends on TSan's own schedule: with the lock removed,
// one round under GOMAXPROCS=1 was caught in 1 of 10 whole-package runs, while
// replaying ten rounds raised that to 9 of 10 (both measured against a build
// with the stateMu publication reverted). The detector reports a race if any
// round observes one, so repetition is what keeps the signal reliable where
// CPUs are scarce.
const stopDuringBootstrapRounds = 10

// TestStopRacingBootstrapPublicationIsSerialized pins the synchronization
// between the parent-context stop path and bootstrap's publication of the task
// runtime.
//
// A parent-context cancellation reaches requestStop on the AfterFunc goroutine
// while the main goroutine is still inside bootstrap, so requestStop's read of
// a.tasks and bootstrap's write of it are only ordered if both take stateMu.
// The behavioural consequence of a missing lock is invisible -- unwind closes
// admission as an invariant either way, which
// TestUnwindClosesAdmissionWhenTheStopPrecedesTheTaskRuntime pins -- so the
// discriminating signal is the race detector: under `go test -race`, a build
// without the lock reports a data race between this read and that write. Only
// a -race run carries that signal: `make check` is race-free, and CI adds a
// root-module race pass of its own.
//
// Being a race-detector test, it must arrange the one thing the detector needs:
// no happens-before edge in either direction between the read and the write.
// The read has to happen while the publishing goroutine is provably between
// bootstrap's entry and its write. The file is a FIFO used as --config, which
// makes both halves observable rather than guessed:
//
//   - A FIFO cannot be opened for writing before a reader opens it, so the
//     successful open below is proof -- not a sleep -- that Execute is parked
//     inside configuration loading.
//   - A reader sees no EOF while a writer holds the FIFO open, so writing the
//     configuration first and closing the FIFO last puts the payload transfer
//     before the stop and the final, EOF-returning read after it. The close is
//     what releases the bootstrap goroutine to publish.
//
// The order of those transfers is not incidental: the syscall package merges a
// clock into the FIFO on every write and acquires it on every successful read,
// so a configuration written after the stop would hand the stop's state to the
// publishing goroutine and order the very pair under test. The first version
// of this test wrote the payload after requestStop and silently passed against
// the unlocked build. Delivering the payload before the stop keeps the only
// carried edge pre-stop, and close carries no edge at all.
//
// The read of a.tasks therefore precedes the publication in time while
// remaining unordered with respect to it, which is exactly the pattern the
// detector flags. The stateMu publication turns it into an ordinary ordered
// pair. The stop here is issued directly by the test goroutine, standing in
// for the AfterFunc goroutine above: what the detector judges is the pair of
// accesses, which the stand-in preserves.
//
// This file builds only where syscall.Mkfifo exists: unix, minus aix and
// solaris (illumos satisfies the solaris tag as well).
func TestStopRacingBootstrapPublicationIsSerialized(t *testing.T) {
	// Replayed because one round is a single draw from the detector's
	// schedule; see stopDuringBootstrapRounds.
	for round := range stopDuringBootstrapRounds {
		t.Run(fmt.Sprintf("round-%d", round+1), stopDuringBootstrap)
	}
}

// stopDuringBootstrap plays one round of the window: it starts an application
// whose configuration arrives through a FIFO, delivers the configuration but
// not the EOF, stops the application while it is parked in the final read, and
// only then releases it by closing the FIFO. The assertions cover what the run
// does when the stop wins: the abort names the phase it interrupted, nothing
// was constructed, and the runtime is still published.
func stopDuringBootstrap(t *testing.T) {
	t.Helper()

	fifoPath := filepath.Join(t.TempDir(), "application.yml")
	require.NoError(t, syscall.Mkfifo(fifoPath, 0o600))

	recorder := &readinessRecorder{}
	app := newRuntimeTestApp(readinessDefinition(recorder, "race-window", nil, readinessStages(recorder, "race-window")))
	result := executeRuntimeTest(app, "--config", fifoPath)

	writer, err := openFIFOWriterWithin(t, fifoPath, result, runtimeTestTimeout)
	require.NoError(t, err, "Execute never reached configuration loading")
	t.Cleanup(func() { _ = writer.Close() })

	// Delivered, but not terminated: Execute consumes the configuration and
	// keeps waiting for more, so the publication cannot happen yet.
	_, err = writer.Write([]byte(runtimeTestConfigContents(time.Second)))
	require.NoError(t, err)

	// The stop lands while Execute is still parked in the configuration read,
	// so this read of a.tasks is unordered with the later publication.
	app.requestStop(stopReasonSignal)

	require.NoError(t, writer.Close())

	completed := awaitRuntimeTestResult(t, result)
	require.Error(t, completed.err, "a stop that lands during bootstrap is a failed startup, not a clean exit")
	assert.Equal(t, 1, completed.code)
	assert.Contains(t, completed.err.Error(), "bootstrapping", "the error names the phase the stop interrupted")
	assert.Contains(t, completed.err.Error(), stopReasonSignal, "the error names why startup was abandoned")
	assert.Empty(t, recorder.snapshot(), "the stop preceded both planning and construction")

	// The abort returns before unwind -- nothing was constructed, so there is
	// nothing to tear down -- and the runtime is still published. Admission is
	// deliberately not asserted here: the admitted-task invariant belongs to
	// unwind, which TestUnwindClosesAdmissionWhenTheStopPrecedesTheTaskRuntime
	// pins, and on this path no plugin ever exists to submit one.
	require.NotNil(t, app.tasks, "bootstrap publishes the task runtime even though a stop is already pending")
}

// openFIFOWriterWithin opens path for writing, waiting until the application
// has opened it for reading. A FIFO cannot be opened for writing before a
// reader exists -- the open fails with ENXIO -- so a successful open is a
// handshake with the reader rather than a guess about its progress. The
// deadline turns "bootstrap never reached configuration loading" into a test
// failure instead of a process that blocks forever, and result is watched so
// that an Execute which finishes -- or fails -- before it ever opens the FIFO
// is reported as itself instead of as a handshake timeout.
func openFIFOWriterWithin(t *testing.T, path string, result <-chan runtimeTestResult, timeout time.Duration) (*os.File, error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		writer, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			return writer, nil
		}
		if !errors.Is(err, syscall.ENXIO) {
			return nil, err
		}
		select {
		case completed := <-result:
			return nil, fmt.Errorf("xbc: test: Execute finished before it opened %s for reading: code %d, err %v", path, completed.code, completed.err)
		default:
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("xbc: test: nothing opened %s for reading within %s", path, timeout)
		}
		time.Sleep(time.Millisecond)
	}
}
