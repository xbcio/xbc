package log

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

// nowFunc is the package-wide time hook, only ever replaced in tests.
//
// It's a package-level variable, so setNow's replacement is not atomic:
// tests that use it must not call t.Parallel(). There is currently no
// t.Parallel() anywhere in log/, so this is safe today — but it's an
// implicit precondition. trace_test.go already reuses this same setNow
// helper (it is not scoped to rotate_test.go); if a future test introduces
// parallelism, it becomes a data race.
var nowFunc = time.Now

// dailyRotator adds day-based rotation on top of lumberjack.
//
// lumberjack itself only rotates by file size, but its Rotate() is
// exported — so this type does exactly one thing: check for a day change
// before every write, and trigger one Rotate() if the day changed.
// Cleanup, compression, and backup count are all handled by lumberjack
// per its existing configuration.
//
// Known behavior:
//   - The archive file name produced by a day rotation carries the
//     timestamp of the moment the rotation was triggered
//     (app-2026-08-25T00-00-03.000.log), while its content belongs to
//     the previous day. This is lumberjack's own naming convention, and
//     max_age is computed against that timestamp too.
//   - There is an attribution-fuzziness window right at the day
//     boundary: when Write detects the day change and calls Rotate(), it
//     does not hold a lock that also covers the actual disk write
//     (lumberjack.Write has its own internal lock, outside d.mu). So the
//     physical location a log line lands in can disagree with its
//     logical day — a line that logically belongs to "today" may end up
//     in "tomorrow's" file. This does not lose data and never writes
//     into an already-archived file; it is purely an ordering/attribution
//     issue, and it is not specific to this implementation — any
//     day-rotation scheme that doesn't hold a lock across the entire
//     write has this same fuzzy window. See the comment in Write for
//     detail.
type dailyRotator struct {
	lj *lumberjack.Logger

	mu  sync.Mutex
	day string
}

func newDailyRotator(lj *lumberjack.Logger) *dailyRotator {
	// [SEC-INFO] The log directory must be 0750. lumberjack hardcodes
	// 0755 when it creates the directory itself (verified: -rwxr-xr-x),
	// which violates the security requirement, so we create it ourselves
	// first: os.MkdirAll returns nil without changing permissions when
	// the directory already exists, so lumberjack's own MkdirAll call
	// later becomes a no-op. Errors here are ignored — if the directory
	// truly can't be created, lumberjack's first write will surface the
	// real reason.
	//
	// Known remaining gap (intentionally not fixed): MkdirAll only
	// actually creates the directory when it doesn't exist yet, so the
	// 0750 guarantee only applies to first creation — if dir already
	// exists with a looser mode (e.g. 0755, pre-created by an ops script,
	// a previous version, or a shared path), we do NOT os.Chmod it to
	// tighten it. Not tightening is a deliberate decision: dir is
	// filepath.Dir(lj.Filename), which depends on the user-configured
	// log.file.path and can well be a system-shared directory like
	// /var/log — the framework forcibly chmod'ing that to 0750 would
	// lock out other services on the same host running as a different
	// user (syslog, filebeat, promtail, and the like), causing a much
	// larger blast radius than the exposure it prevents, and one that's
	// very hard to diagnose. The residual risk is limited to file names,
	// sizes, and mtimes inside the directory being visible to other
	// local users — content itself is unaffected, see the tightening of
	// lj.Filename itself below. This is a deployment precondition: ops
	// must ensure the log directory itself is created with 0750 or
	// stricter (see README.md's "部署前提：日志目录权限" section).
	_ = os.MkdirAll(filepath.Dir(lj.Filename), 0o750)

	// [SEC-INFO] lumberjack's openNew() creates a brand-new file with
	// 0600, but if lj.Filename already exists it copies the old file's
	// mode instead (lumberjack.go:219, `mode = info.Mode()`) rather than
	// tightening it to 0600; and on the next rotation, the gzip archive
	// step inherits that same looser mode too (lumberjack.go:486 also
	// uses the old file's fi.Mode()). This is the real content-exposure
	// gap: if app.log was previously created at 0644 by an ops script or
	// an older version, without tightening it here it stays 0644 forever
	// and any other local user can read the log content directly.
	// lj.Filename is a file that unambiguously belongs to this framework
	// (unlike the shared directory case above), so chmod'ing it doesn't
	// carry that collateral-damage risk — hence we tighten it ourselves
	// before handing off to lumberjack. When the file doesn't exist yet,
	// we do nothing — lumberjack will create it at 0600 on its own.
	//
	// The os.Stat guard (err == nil && Perm() != 0o600) serves two
	// purposes: (1) skip the chmod when the file doesn't exist (nothing
	// to tighten; lumberjack will create it correctly), and (2) skip the
	// syscall when the file is already at 0600 (pure efficiency — the
	// behavioral outcome is identical to an unconditional os.Chmod, but
	// we avoid one syscall on the common "file already correct" path).
	// Tests cannot distinguish these two implementations on a real
	// filesystem because MkdirAll+Chmod failures would be swallowed, and
	// the end state is the same either way.
	//
	// Errors are ignored: a failed chmod means the file retains its
	// original (looser) permissions. This is a degraded-security state,
	// not a fatal error — continuing to write logs at a looser permission
	// is preferable to refusing to log entirely. The exposure is limited
	// to other local users being able to read log content until the file
	// is next rotated (at which point lumberjack creates the new file at
	// 0600).
	if fi, err := os.Stat(lj.Filename); err == nil && fi.Mode().Perm() != 0o600 {
		_ = os.Chmod(lj.Filename, 0o600)
	}

	return &dailyRotator{lj: lj, day: nowFunc().Format(time.DateOnly)}
}

func (d *dailyRotator) Write(p []byte) (int, error) {
	d.mu.Lock()
	if today := nowFunc().Format(time.DateOnly); today != d.day {
		d.day = today
		// Skip rotation on an empty file: Rotate() still produces a
		// 0-byte archive for a 0-byte current file (verified), so a
		// process's first write of a new day would otherwise leave this
		// kind of junk archive behind, accumulating over time.
		if fi, err := os.Stat(d.lj.Filename); err != nil || fi.Size() > 0 {
			// A failed rotation must not block writes: a log that fails
			// to rotate is an ops problem, a log that gets dropped is an
			// incident.
			_ = d.lj.Rotate()
		}
	}
	d.mu.Unlock()

	// lumberjack.Write has its own internal lock, and this call is
	// deliberately placed outside d.mu — we don't want to widen d.mu to
	// cover the entire disk write.
	//
	// The cost of that choice (measured under a review's 200-goroutine
	// stress test with deterministic interleaving, not a hypothetical):
	// writer C can finish its "same day, no rotation needed" check under
	// d.mu, then get descheduled before it actually calls lj.Write();
	// meanwhile writer A detects the day change, calls Rotate(), and
	// releases d.mu. Only then does C's lj.Write() actually run — and it
	// lands in the file created by A's Rotate(), i.e. the new day's file,
	// mixed in with the new day's content. No data is lost and nothing
	// is ever written into an already-archived file — this is purely an
	// ordering/attribution issue right at the day boundary: a line whose
	// logical timestamp is "today" can end up physically stored in
	// "tomorrow's" file. This is not a defect unique to this
	// implementation; any day-rotation scheme that doesn't hold a single
	// lock across the entire write has this same fuzzy window.
	return d.lj.Write(p)
}

// Sync satisfies zapcore.WriteSyncer. lumberjack writes directly to the
// fd without buffering, so there's nothing to do here.
//
// Note that lumberjack.Logger does NOT have a Sync method (its method
// set is only Close/Rotate/Write) — don't try to forward a method that
// doesn't exist.
func (d *dailyRotator) Sync() error { return nil }

func (d *dailyRotator) Close() error { return d.lj.Close() }
