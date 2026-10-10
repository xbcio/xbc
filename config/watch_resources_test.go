package config

import (
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestWatchFilesKeepsOneArmedWatcherPerFile pins the resource discipline of a
// standing watch: one armed provider per file for as long as its loop runs,
// not a new one built on every re-arm cycle and abandoned mid-loop.
//
// The koanf provider arms asynchronously -- (*File).Watch registers the
// watcher and returns, and the loop that reports runs on the provider's own
// goroutine -- so a re-arm loop that treats Watch as blocking builds a fresh
// watcher every cycle and leaves every previous one running, growing the
// process by a provider goroutine and an fsnotify watcher per cycle for as
// long as the watch lives. The goroutine count is what separates the two
// shapes: a standing watch must not accumulate goroutines while it watches,
// and a stopped watch must not leave any behind.
//
// The assertions are on growth, not on an exact count: what runs beside the
// watch (the test framework, the earlier tests in this package) is not this
// test's to predict, and the leak this pins is one of the largest possible --
// two goroutines per re-arm cycle for the life of the process.
func TestWatchFilesKeepsOneArmedWatcherPerFile(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "application.yml")
	writeYAML(t, base, "a: 1\n")

	settleGoroutines()
	baseline := runtime.NumGoroutine()

	stop, err := WatchFiles([]string{base}, func([]string) {})
	require.NoError(t, err)

	// Let the arm land, then take the size of an armed watch.
	time.Sleep(200 * time.Millisecond)
	armed := runtime.NumGoroutine()
	require.GreaterOrEqual(t, armed, baseline+1, "an armed watch must be running something")

	// Several re-arm periods go by with nothing happening to the file. A
	// watch that rebuilds its provider on a timer adds two goroutines per
	// period here; one that keeps its provider adds none.
	time.Sleep(5 * watchRearm)
	require.LessOrEqual(t, runtime.NumGoroutine(), armed+2,
		"a standing watch must not accumulate watcher goroutines")

	require.NoError(t, stop())
	require.Eventually(t, func() bool {
		return runtime.NumGoroutine() <= baseline+1
	}, 2*time.Second, 20*time.Millisecond,
		"a stopped watch must not leave its provider goroutines behind")
}

// settleGoroutines gives goroutines that are already on their way out a
// chance to leave, so a count taken here is a stable base.
func settleGoroutines() {
	for i := 0; i < 10; i++ {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
}

// TestWatchFilesSerializesCallbacks pins the promise WatchFiles documents:
// changed is never called concurrently with itself. The callback here is slow
// relative to the debounce, so filesystem events pile up behind it and the
// watcher is repeatedly due to report while a report is still running -- the
// exact shape whose correct handling is to defer the next report rather than
// run it beside the first.
func TestWatchFilesSerializesCallbacks(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "application.yml")
	writeYAML(t, base, "a: 1\n")

	oldDebounce, oldRearm := watchDebounce, watchRearm
	watchDebounce, watchRearm = 20*time.Millisecond, 10*time.Millisecond
	defer func() { watchDebounce, watchRearm = oldDebounce, oldRearm }()

	var mu sync.Mutex
	running, maxRunning, calls := 0, 0, 0
	stop, err := WatchFiles([]string{base}, func([]string) {
		mu.Lock()
		running++
		calls++
		if running > maxRunning {
			maxRunning = running
		}
		mu.Unlock()

		time.Sleep(120 * time.Millisecond)

		mu.Lock()
		running--
		mu.Unlock()
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, stop()) }()

	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		writeYAML(t, base, "a: 2\n")
		time.Sleep(15 * time.Millisecond)
	}

	mu.Lock()
	observedCalls, observedMax := calls, maxRunning
	mu.Unlock()
	require.GreaterOrEqual(t, observedCalls, 2, "the assertion needs more than one callback to say anything")
	require.Equal(t, 1, observedMax, "changed must never run concurrently with itself")
}
