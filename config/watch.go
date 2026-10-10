package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/knadh/koanf/providers/file"
)

// watchDebounce is how long the watcher waits after the last filesystem event
// before it reports a change. One save can produce several events (a truncate,
// a write, a chmod), and an editor may replace a file through a temp file and
// a rename; reporting each one would turn a single save into a reload storm.
//
// Variables rather than constants so this package's own tests can shorten
// them; nothing outside the package can. Tests that change them restore the
// originals when they stop their watch, and no test runs in parallel.
var watchDebounce = 100 * time.Millisecond

// watchRearm is how long a file's watch waits before arming itself again. The
// koanf file provider ends its watch loop when the file is removed, so a
// process that only watched once would go deaf after the first deletion or
// atomic replacement -- the ConfigMap update flow this exists to serve. The
// same delay paces the retry loop for a file that is configured but not on
// disk yet.
var watchRearm = 250 * time.Millisecond

// WatchFiles watches paths and calls changed with the paths whose content
// may now differ, after changes have settled for watchDebounce. It watches the
// configuration files a deployment mounted, so the runtime can reload what
// they say; the callback is a notification, not a result -- whatever reloads
// reads the files again itself.
//
// The paths are the ones Environment.Files returns. A file may be absent when
// the watch starts and appear later: the watch arms itself on the parent
// directory, which is also how it follows a symlinked file to whatever target
// a ConfigMap swap points it at. Every parent directory must exist, because a
// watch armed nowhere would never report anything; a path without one is
// refused at the call.
//
// A removal and a restoration are both reported; a restoration that lands in
// the re-arm gap (under watchRearm) is reported by the round after it, so a
// change can be noticed late, but never silently dropped.
//
// The returned stop function unwatches everything and waits for a callback
// already in flight; it is idempotent. changed is never called after stop
// returns, and never concurrently with itself.
func WatchFiles(paths []string, changed func(changed []string)) (stop func() error, err error) {
	w := newFileWatch(paths, changed)
	if err = w.start(); err != nil {
		_ = w.stop()
		return nil, err
	}
	return w.stop, nil
}

// fileWatch is the mutable half: one re-arming watch per file, one debounce
// timer for all of them. Every execution path that touches its state from a
// watcher goroutine holds mu.
type fileWatch struct {
	paths   []string
	changed func([]string)

	mu       sync.Mutex
	stopping bool
	pending  bool
	running  bool
	timer    *time.Timer

	done     chan struct{}
	joined   sync.WaitGroup // the per-file goroutines
	callback sync.WaitGroup // the debounce timer's callback while it runs

	// stopOnce keeps stop's one-shot work from running twice; stopMu keeps
	// concurrent stop calls from interleaving their waits.
	stopOnce sync.Once
	stopMu   sync.Mutex
}

func newFileWatch(paths []string, changed func([]string)) *fileWatch {
	if changed == nil {
		changed = func([]string) {}
	}
	// Copy: the caller's slice is its own, and the watcher outlives this call.
	return &fileWatch{
		paths:   append([]string(nil), paths...),
		changed: changed,
		done:    make(chan struct{}),
	}
}

func (w *fileWatch) start() error {
	// Every path is resolved to an absolute, cleaned form up front: the
	// provider cleans what it is given, and the callback must hand back the
	// same spelling the probe arrives in -- relative spellings would miss.
	for i, path := range w.paths {
		abs, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("xbc: cannot watch configuration file %s: %w", path, err)
		}
		if _, err := os.Stat(filepath.Dir(abs)); err != nil {
			return fmt.Errorf("xbc: cannot watch configuration file %s: its directory is not accessible: %w", path, err)
		}
		w.paths[i] = abs
	}

	// The timer is created before any watch starts: an event arriving before
	// it would find w.timer nil on the first reset.
	w.timer = time.AfterFunc(watchDebounce, w.fire)

	for _, path := range w.paths {
		w.watchOne(path)
	}
	return nil
}

// watchOne arms the watch on path's parent directory and keeps it armed.
//
// The koanf file provider arms asynchronously: (*File).Watch registers the
// watcher and returns, and the loop that actually reports runs on the
// provider's own goroutine until the file is removed or something inside the
// watch fails. A round here is therefore one armed provider kept for as long
// as its loop lasts, with the loop's end -- delivered to the callback as an
// error -- as the signal to arm the next one. Re-arming on a timer instead
// builds a fresh watcher every cycle and abandons the previous one mid-loop,
// which grows the process by a provider goroutine and an fsnotify watcher per
// cycle, forever.
//
// Instances are used once and only after a successful arm are they ever
// touched again. The arm can poison an instance: when the file is absent,
// Watch's symlink resolution returns with the instance's internal lock still
// held, so a second Watch or an Unwatch on that instance blocks forever. A
// round therefore builds a fresh provider, and stop unwatches only an
// instance whose arm succeeded.
func (w *fileWatch) watchOne(path string) {
	w.joined.Add(1)
	go func() {
		defer w.joined.Done()
		for {
			provider := file.Provider(path)
			ended := make(chan struct{})
			var endedOnce sync.Once

			armErr := provider.Watch(func(_ any, err error) {
				select {
				case <-w.done:
					return
				default:
				}
				// The caller hears about every callback, an ending one
				// included: err non-nil means the provider's loop is over --
				// the file was removed, or something inside the watch failed
				// -- and what to read next is up to the caller.
				w.changedUnderWatch()
				if err != nil {
					endedOnce.Do(func() { close(ended) })
				}
			})
			if armErr != nil {
				// Not armed: the file is not on disk yet, or the parent
				// directory could not be watched. The instance is poisoned
				// and must not be touched again; fall through to the re-arm
				// wait, which is also the retry loop for a file that has not
				// materialized yet.
				endedOnce.Do(func() { close(ended) })
			}

			select {
			case <-ended:
			case <-w.done:
				// Stopping while the loop still runs. This instance is armed
				// and healthy, and only Unwatch ends its goroutine -- nothing
				// else will, because its file is still there.
				if armErr == nil {
					_ = provider.Unwatch()
				}
				return
			}
			select {
			case <-w.done:
				return
			default:
			}

			// The gap to the next arm is a window in which the file can be
			// written with no watch on it -- exactly what a ConfigMap-style
			// swap does -- so a file that is back when the gap ends is
			// reported as the change it is. Deciding before the wait is what
			// separates that from a file that was simply never there, which
			// reports nothing: a caller whose reload watches a
			// not-yet-materialized file must not be told to reload for the
			// condition it already read.
			_, errBefore := os.Stat(path)

			select {
			case <-time.After(watchRearm):
			case <-w.done:
				return
			}
			select {
			case <-w.done:
				return
			default:
			}

			if errBefore != nil {
				if _, errAfter := os.Stat(path); errAfter == nil {
					select {
					case <-w.done:
						return
					default:
					}
					w.changedUnderWatch()
				}
			}
		}
	}()
}

// changedUnderWatch reports a change, collapsing events that arrive while a
// callback is running or the debounce has not elapsed.
func (w *fileWatch) changedUnderWatch() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopping || w.pending {
		// Stopping: the report is no longer wanted. Pending: either a callback
		// is in flight or a burst is being debounced; the timer already
		// running covers this event.
		return
	}
	w.pending = true
	w.timer.Reset(watchDebounce)
}

// fire runs one callback at a time, then reopens the collapse window. The
// callback runs without the lock so a slow reload cannot make the watcher
// deaf to fsnotify events: those are buffered by the provider and by pending,
// not by blocking this goroutine.
//
// A report that comes due while a callback is running is left owed rather
// than run beside it: the running callback re-arms the timer as it finishes,
// so a slow reload sees the events that arrived during it as one follow-up
// report, and changed is never called concurrently with itself.
func (w *fileWatch) fire() {
	w.mu.Lock()
	if w.stopping {
		w.mu.Unlock()
		return
	}
	if w.running {
		// Owed to the callback in flight; its completion re-arms the timer.
		w.pending = true
		w.mu.Unlock()
		return
	}
	w.pending = false
	w.running = true
	// Registered under the lock, so stop cannot miss a callback that has
	// already passed the stopping check.
	w.callback.Add(1)
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		w.running = false
		if w.pending && !w.stopping {
			w.pending = false
			w.timer.Reset(watchDebounce)
		}
		w.mu.Unlock()
		w.callback.Done()
	}()
	w.changed(append([]string(nil), w.paths...))
}

func (w *fileWatch) stop() error {
	w.stopMu.Lock()
	defer w.stopMu.Unlock()

	w.stopOnce.Do(func() {
		w.mu.Lock()
		w.stopping = true
		if w.timer != nil { // stop also runs when start refused before arming it
			w.timer.Stop()
		}
		w.mu.Unlock()

		// done first: it releases every re-arm loop and the provider callback
		// closuring over it, which is what ends the current rounds -- this
		// does not Unwatch them, for the reason in watchOne. Then the
		// goroutines, and only then the callback, which is the reason a stop
		// returns rather than racing out from under an in-flight reload.
		close(w.done)
		w.joined.Wait()
		w.callback.Wait()
	})
	return nil
}
