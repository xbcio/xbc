package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFilesNamesTheConfigurationFilesInMergeOrder(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "application.yml")
	profile := filepath.Join(dir, "application-prod.yml")
	writeYAML(t, base, "server:\n  addr: \":9000\"\n")
	writeYAML(t, profile, "server:\n  addr: \":9100\"\n")

	env, err := Load(Options{File: base, Profile: "prod"})
	require.NoError(t, err)

	assert.Equal(t, []string{base, profile}, env.Files(),
		"the files are the ones the merge read, lowest precedence first: the base file, then the profile overlay")
}

// A profile that is configured but not on disk yet is still part of the
// watched set: the file may appear after the process starts (a mounted
// overlay), and that is a change the watcher owes its caller.
func TestFilesIncludeAConfiguredProfileThatDoesNotExistYet(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "application.yml")
	writeYAML(t, base, "server:\n  addr: \":9000\"\n")

	env, err := Load(Options{File: base, Profile: "prod"})
	require.NoError(t, err)

	assert.Equal(t, []string{base, filepath.Join(dir, "application-prod.yml")}, env.Files())
}

func TestFilesIsEmptyWithoutAFileLayer(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	env, err := Load(Options{})
	require.NoError(t, err)
	assert.Empty(t, env.Files(), "a tree assembled without a file layer has nothing to watch")

	mem, err := NewEnvironment(map[string]any{"a": 1}, DefaultEnvPrefix)
	require.NoError(t, err)
	assert.Empty(t, mem.Files(), "the in-memory environment never reads a file")
}

// startWatching lowers the watcher's timing knobs for the duration of one test
// and starts a watch over files, stopping both when the test ends. The knobs
// are package variables rather than API so that production keeps its values
// without every caller growing a timing surface.
func startWatching(t *testing.T, paths []string) (<-chan []string, func()) {
	t.Helper()

	oldDebounce, oldRearm := watchDebounce, watchRearm
	watchDebounce, watchRearm = 20*time.Millisecond, 10*time.Millisecond

	changes := make(chan []string, 16)
	stop, err := WatchFiles(paths, func(changed []string) {
		select {
		case changes <- changed:
		default:
		}
	})
	require.NoError(t, err)

	stopped := false
	stopOnce := func() {
		if stopped {
			return
		}
		stopped = true
		require.NoError(t, stop())
		watchDebounce, watchRearm = oldDebounce, oldRearm
	}
	t.Cleanup(stopOnce)
	return changes, stopOnce
}

func TestWatchFilesReportsAWrite(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "application.yml")
	writeYAML(t, base, "a: 1\n")

	changes, _ := startWatching(t, []string{base})

	writeYAML(t, base, "a: 2\n")

	select {
	case got := <-changes:
		assert.Equal(t, []string{base}, got)
	case <-time.After(2 * time.Second):
		t.Fatal("a write to a watched file must reach the callback")
	}
}

// The atomic-replacement shape (write a temp file, rename over the target) is
// what kubectl and os.CreateTemp+rename flows produce; the watch has to
// survive it and report the replacement.
func TestWatchFilesSurvivesAtomicReplacement(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "application.yml")
	writeYAML(t, base, "a: 1\n")

	changes, _ := startWatching(t, []string{base})

	tmp := filepath.Join(dir, "tmp.yml")
	writeYAML(t, tmp, "a: 2\n")
	require.NoError(t, os.Rename(tmp, base))

	select {
	case got := <-changes:
		assert.Equal(t, []string{base}, got)
	case <-time.After(2 * time.Second):
		t.Fatal("an atomic replacement must reach the callback")
	}
}

// Follow the symlink, not the name: a ConfigMap swap replaces the target and
// retargets the link, and the watch must report the file whose content
// changed.
func TestWatchFilesFollowsASymlinkToItsTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "..data-1.yml")
	link := filepath.Join(dir, "application.yml")
	writeYAML(t, target, "a: 1\n")
	require.NoError(t, os.Symlink(target, link))

	changes, _ := startWatching(t, []string{link})

	// The swap: a second target, then a new link pointing at it.
	next := filepath.Join(dir, "..data-2.yml")
	writeYAML(t, next, "a: 2\n")
	replacement := filepath.Join(dir, "..data-link")
	require.NoError(t, os.Symlink(next, replacement))
	require.NoError(t, os.Rename(replacement, link))

	select {
	case got := <-changes:
		assert.Equal(t, []string{link}, got)
	case <-time.After(2 * time.Second):
		t.Fatal("a symlink swap must reach the callback")
	}
}

// A relative path is the shape the runtime actually hands over: --config
// application.yml resolves through the documented lookup to a relative path.
// The daemon's working directory can change only if someone calls chdir, which
// a service has no business doing -- but the watch outlives the call, so it
// works from the absolute path it resolved instead of looking the name up
// again on every event.
func TestWatchFilesResolvesRelativePathsAgainstTheStartupDirectory(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "application.yml")
	writeYAML(t, base, "a: 1\n")

	t.Chdir(dir) // before WatchFiles: the lookup must happen against this directory
	changes, _ := startWatching(t, []string{"application.yml"})

	// Resolved the same way the watcher resolves it -- t.Chdir's directory can
	// itself sit behind a symlink (/var on macOS), so the expected value is
	// this expression, not the t.TempDir spelling.
	resolved, err := filepath.Abs("application.yml")
	require.NoError(t, err)

	writeYAML(t, base, "a: 2\n")
	select {
	case got := <-changes:
		assert.Equal(t, []string{resolved}, got, "the callback reports the resolved path, not the spelling it was given")
	case <-time.After(2 * time.Second):
		t.Fatal("a relative path must be watched through its resolved absolute form")
	}
}

// A file that is not on disk when the watch starts -- a mounted overlay that
// has not materialized yet -- must be picked up when it appears. Nothing is
// reported while it is absent, because the caller's read of the configuration
// already saw it that way.
func TestWatchFilesPicksUpAFileThatAppearsLater(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "application.yml")

	changes, _ := startWatching(t, []string{base})

	writeYAML(t, base, "a: 1\n")
	select {
	case <-changes:
	case <-time.After(2 * time.Second):
		t.Fatal("a file that appears after the watch started must reach the callback")
	}

	// And the watch that started this way keeps working.
	writeYAML(t, base, "a: 2\n")
	select {
	case <-changes:
	case <-time.After(2 * time.Second):
		t.Fatal("the watch must keep reporting after the file appeared")
	}
}

// A removal kills the fsnotify watch (the provider's loop ends with the file),
// so the watcher has to re-arm: the file coming back must be reported again,
// and the watch after that still has to work.
func TestWatchFilesReArmsAfterARemoval(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "application.yml")
	writeYAML(t, base, "a: 1\n")

	changes, _ := startWatching(t, []string{base})

	require.NoError(t, os.Remove(base))
	// Wait for the removal itself before restoring the file: the callback is
	// what tells the test the old watch is gone, so the write below lands in
	// the re-arm gap rather than racing it.
	select {
	case <-changes:
	case <-time.After(2 * time.Second):
		t.Fatal("the removal must reach the callback")
	}

	writeYAML(t, base, "a: 3\n")
	select {
	case <-changes:
	case <-time.After(2 * time.Second):
		t.Fatal("the watch must be re-armed after the file was removed")
	}

	// And the re-armed watch keeps working for later writes.
	writeYAML(t, base, "a: 4\n")
	select {
	case <-changes:
	case <-time.After(2 * time.Second):
		t.Fatal("the re-armed watch must keep reporting later writes")
	}
}

// One burst of writes is one reload request: the debounce is what keeps an
// editor's save (or a formatter's several writes) from compiling into a
// reload storm.
func TestWatchFilesCollapsesABurstIntoOneCallback(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "application.yml")
	writeYAML(t, base, "a: 0\n")

	oldDebounce, oldRearm := watchDebounce, watchRearm
	watchDebounce, watchRearm = 150*time.Millisecond, 10*time.Millisecond
	calls := make(chan []string, 16)
	stop, err := WatchFiles([]string{base}, func(changed []string) {
		select {
		case calls <- changed:
		default:
		}
	})
	require.NoError(t, err)
	defer func() {
		require.NoError(t, stop())
		watchDebounce, watchRearm = oldDebounce, oldRearm
	}()

	for i := 1; i <= 8; i++ {
		writeYAML(t, base, "a: "+string(rune('0'+i))+"\n")
	}

	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("a write burst must produce a callback")
	}
	time.Sleep(3 * watchDebounce)
	select {
	case <-calls:
		t.Fatal("the burst must collapse into one callback")
	default:
	}
}

func TestWatchFilesStopIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "application.yml")
	writeYAML(t, base, "a: 1\n")

	stop, err := WatchFiles([]string{base}, func([]string) {})
	require.NoError(t, err)

	require.NoError(t, stop())
	require.NoError(t, stop(), "stopping twice must be safe")
}

// Refusing up front is deliberate: a path whose parent directory does not
// exist cannot be watched at all (the directory is where the watch is armed,
// and the file appearing is what the caller is waiting for), and a caller
// that never gets a callback would have no way to tell.
func TestWatchFilesRefusesAPathWhoseParentDirectoryIsMissing(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "not-there", "application.yml")

	_, err := WatchFiles([]string{missing}, func([]string) {})
	require.Error(t, err, "an unwatchable path must fail at the call, not silently never fire")
}

func TestWatchFilesWithNoFilesIsANoOpStop(t *testing.T) {
	stop, err := WatchFiles(nil, func([]string) {})
	require.NoError(t, err)
	require.NoError(t, stop())
}
