package log

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

// setNow replaces the package-wide time hook and returns a restore function.
// Task 8's Span tests reuse it too.
//
// IMPORTANT: this replaces nowFunc (rotate.go), which is a plain
// package-level variable — the replacement is not atomic. Tests using
// setNow must not call t.Parallel(). See the comment on nowFunc for the
// full rationale.
func setNow(f func() time.Time) (restore func()) {
	old := nowFunc
	nowFunc = f
	return func() { nowFunc = old }
}

func TestDailyRotatorTriggersOnDayChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	fake := time.Date(2026, 8, 24, 23, 59, 0, 0, time.Local)
	defer setNow(func() time.Time { return fake })()

	d := newDailyRotator(&lumberjack.Logger{Filename: path, MaxBackups: 3, LocalTime: true})

	_, err := d.Write([]byte("day1\n"))
	require.NoError(t, err)

	fake = fake.Add(2 * time.Minute) // crosses the day boundary
	_, err = d.Write([]byte("day2\n"))
	require.NoError(t, err)
	require.NoError(t, d.Close())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 2, "Should have an archive file and a current file after day change")

	cur, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "day2\n", string(cur), "Current file contains only content after day change")
}

func TestDailyRotatorDoesNotRotateWithinSameDay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	fake := time.Date(2026, 8, 24, 8, 0, 0, 0, time.Local)
	defer setNow(func() time.Time { return fake })()

	d := newDailyRotator(&lumberjack.Logger{Filename: path, LocalTime: true})

	_, err := d.Write([]byte("a\n"))
	require.NoError(t, err)
	fake = fake.Add(13 * time.Hour) // moves most of the day forward, still same day
	_, err = d.Write([]byte("b\n"))
	require.NoError(t, err)
	require.NoError(t, d.Close())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "Do not roll within the same day")

	cur, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "a\nb\n", string(cur))
}

func TestDailyRotatorConcurrentWritesRotateOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	fake := time.Date(2026, 8, 24, 23, 59, 59, 0, time.Local)
	var mu sync.Mutex
	defer setNow(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return fake
	})()

	d := newDailyRotator(&lumberjack.Logger{Filename: path, MaxBackups: 10, LocalTime: true})
	_, err := d.Write([]byte("seed\n"))
	require.NoError(t, err)

	mu.Lock()
	fake = fake.Add(time.Second) // every goroutine observes the day change at once
	mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = d.Write([]byte("x\n"))
		}()
	}
	wg.Wait()
	require.NoError(t, d.Close())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 2, "32 concurrent writes should trigger roll only once")
}

func TestDailyRotatorSyncIsNoop(t *testing.T) {
	d := newDailyRotator(&lumberjack.Logger{Filename: filepath.Join(t.TempDir(), "a.log")})
	assert.NoError(t, d.Sync(), "lumberjack does not buffer, Sync does nothing but must exist to satisfy WriteSyncer")
	assert.NoError(t, d.Close())
}

func TestDailyRotatorSkipsRotateOnEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(path, nil, 0o600)) // current file is 0 bytes

	fake := time.Date(2026, 8, 24, 23, 59, 0, 0, time.Local)
	defer setNow(func() time.Time { return fake })()

	d := newDailyRotator(&lumberjack.Logger{Filename: path, MaxBackups: 3, LocalTime: true})
	fake = fake.Add(2 * time.Minute) // crosses the day boundary
	_, err := d.Write([]byte("day2\n"))
	require.NoError(t, err)
	require.NoError(t, d.Close())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "Empty file across day change should not roll out 0-byte archive")
}

func TestDailyRotatorCreatesDirWith0750(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "logs")
	d := newDailyRotator(&lumberjack.Logger{Filename: filepath.Join(dir, "app.log")})
	defer d.Close()

	fi, err := os.Stat(dir)
	require.NoError(t, err)
	// [SEC-INFO] lumberjack creates the directory itself at 0755, so we
	// must create it ourselves first at 0750.
	assert.Equal(t, os.FileMode(0o750), fi.Mode().Perm(), "log directory permissions")
}

// TestDailyRotatorDoesNotChmodPreexistingDir pins down a deliberate
// decision, not a defect: if the directory already exists with a looser
// mode, we do not tighten it. See the comment on newDailyRotator in
// rotate.go for why — dir may be a system-shared directory like
// /var/log, and the framework forcibly chmod'ing it would affect other
// processes, with a blast radius much larger than the exposure it
// prevents. This only asserts "we don't touch the directory"; content
// safety is backstopped by the file itself being 0600.
//
// Umask dependency: os.Mkdir applies the process umask to the requested
// mode. This test assumes the default umask (0022), which yields the
// expected 0755. If run under a non-standard umask (e.g., 0077), the
// actual directory mode would differ and the assertion would fail. This
// is acceptable because: (1) Go test processes inherit the user's
// default shell umask, which is 0022 on every Linux/macOS system we
// target, and (2) CI environments uniformly use 0022. Explicitly
// setting umask via syscall.Umask(0) is possible but would affect
// other tests sharing the process, and t.Parallel is prohibited in
// this package (see nowFunc's comment).
func TestDailyRotatorDoesNotChmodPreexistingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "preexisting")
	require.NoError(t, os.Mkdir(dir, 0o755))

	d := newDailyRotator(&lumberjack.Logger{Filename: filepath.Join(dir, "app.log")})
	defer d.Close()

	fi, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), fi.Mode().Perm(), "Pre-existing directory permissions should not be modified by us")
}

// TestDailyRotatorTightensPreexistingFileTo0600 covers the real gap
// called out by review: lumberjack's openNew() copies the mode of an
// already-existing file (lumberjack.go:219). If app.log was previously
// created at 0644 by an ops script or an older version, lumberjack won't
// tighten it, and the next rotation will propagate 0644 to the gzip
// archive too (lumberjack.go:486). We must tighten a preexisting file to
// 0600 before lumberjack gets to it.
func TestDailyRotatorTightensPreexistingFileTo0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(path, []byte("Old content\n"), 0o644))

	d := newDailyRotator(&lumberjack.Logger{Filename: path})
	defer d.Close()

	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "Pre-existing log file permissions must be tightened to 0600")
}

// TestDailyRotatorDoesNotCreateFileWhenAbsent confirms the tightening
// logic only applies to a file that already exists: when the file
// doesn't exist, newDailyRotator neither errors nor eagerly creates it
// (creation is still left to lumberjack's first Write, which creates it
// at 0600).
func TestDailyRotatorDoesNotCreateFileWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	d := newDailyRotator(&lumberjack.Logger{Filename: path})
	defer d.Close()

	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err), "File should not be preemptively created when not exists")
}

// TestDailyRotatorSkipsChmodWhenAlready0600 exercises the "file exists
// and already has correct permissions" branch of the os.Stat guard:
// when the file is already 0600, no chmod syscall is needed and the file
// remains unchanged. This cannot distinguish whether the implementation
// actually skipped the syscall (both paths produce the same end state on
// a real filesystem), but it confirms the Stat+Perm check does not
// inadvertently break anything on the common "permissions already
// correct" path.
func TestDailyRotatorSkipsChmodWhenAlready0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(path, []byte("Existing content\n"), 0o600))

	d := newDailyRotator(&lumberjack.Logger{Filename: path})
	defer d.Close()

	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(),
		"File already 0600 should not be modified")
}
