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
// a ConfigMap swap points it at -- as far as the platform reports the swap:
// macOS's kqueue never reports the rename over the link, so a swap there can
// emit no event at all. Every parent directory must exist, because a watch
// armed nowhere would never report anything; a path without one is refused at
// the call.
//
// The call returns once every file's first arm attempt has completed: a file
// already on disk is watched when it returns, and a file that had not
// materialized has its retry loop running. A write issued after the call
// therefore cannot fall into the arming unseen -- an armed file reports it as
// an event, and a file whose arm was refused reports it from the round that
// arms over the change.
//
// A removal and a restoration are both reported; a restoration that lands in
// the re-arm gap (under watchRearm) is reported by the round that re-arms --
// its events went to nobody, so the arm carries the report itself -- and a
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
	//
	// Presence is sampled in the same pass, synchronously, before any watch
	// goroutine starts: it is what tells the first arm apart from every
	// later one. A file that is absent at this instant may appear before the
	// first arm runs, and that appearance is a change the caller is owed;
	// sampled inside the arm goroutine, the answer would land on the wrong
	// side of that race.
	presentAtStart := make([]bool, len(w.paths))
	for i, path := range w.paths {
		abs, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("xbc: cannot watch configuration file %s: %w", path, err)
		}
		if _, err := os.Stat(filepath.Dir(abs)); err != nil {
			return fmt.Errorf("xbc: cannot watch configuration file %s: its directory is not accessible: %w", path, err)
		}
		w.paths[i] = abs
		_, statErr := os.Stat(abs)
		presentAtStart[i] = statErr == nil
	}

	// Every file's first arm attempt completes before start returns. The
	// provider arms asynchronously -- its Watch registers the watcher and
	// returns, and the loop that reports runs on its own goroutine -- but a
	// caller told the watch has started must not have to race that goroutine
	// to have its next write seen: the runtime starts this watch just before
	// it announces the process is running, and a write that follows the
	// announcement has to land on an armed watch. The signal is sent after
	// the attempt, armed or refused; a refusal on a file that is not there
	// yet is a supported outcome, and the retry loop keeps that file live.
	firstArm := make(chan struct{}, len(w.paths))
	for i, path := range w.paths {
		w.watchOne(path, presentAtStart[i], firstArm)
	}
	for range w.paths {
		<-firstArm
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
// error -- as the signal to arm the next one, and a successful arm that has a
// gap behind it reports once: events emitted while nothing watched are gone,
// so the arm itself is the only evidence the caller may have missed a change.
// Re-arming on a timer instead
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
func (w *fileWatch) watchOne(path string, presentAtStart bool, firstArm chan<- struct{}) {
	w.joined.Add(1)
	go func() {
		defer w.joined.Done()
		first := true
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
			if first {
				// Unblocks start: the first attempt is over, armed or
				// refused, and the watcher's state is settled for a caller
				// that writes from here on.
				firstArm <- struct{}{}
			}
			switch {
			case armErr != nil:
				// Not armed: the file is not on disk yet, or the parent
				// directory could not be watched. The instance is poisoned
				// and must not be touched again; fall through to the re-arm
				// wait, which is also the retry loop for a file that has not
				// materialized yet.
				endedOnce.Do(func() { close(ended) })
			case !first || !presentAtStart:
				// Armed over the gap this round is the end of: whatever was
				// written while nothing watched emitted its events to
				// nobody, so the re-arm itself is the report -- the caller
				// re-reads and sees the state the gap produced. The first
				// arm of a watch is the one arm with no gap behind it: it
				// stays silent for a file that was already there, and
				// reports for one that appeared since the watch started --
				// the same change a later round would have carried, before
				// any round existed to carry it.
				w.changedUnderWatch()
			}
			first = false

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

			// The wait is the window in which the file can be written with
			// no watch on it -- exactly what a ConfigMap-style swap does.
			// Nothing is sampled around it: a state read between the loop's
			// end and the next arm races the very writes it is meant to
			// catch, and enough scheduling pressure loses them. The next
			// successful arm reports instead (see above), which needs no
			// sample to be right.
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
		}
	}()
}

// changedUnderWatch reports a change, collapsing events that arrive while a
// callback is running or the debounce has not elapsed.
//
// The timer is created on the first event rather than armed at start: an
// AfterFunc fires when its interval ends whether or not anything happened, so
// a timer armed in start would call back once per watch for a file nobody
// touched -- a spurious reload at every startup.
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
	if w.timer == nil {
		w.timer = time.AfterFunc(watchDebounce, w.fire)
		return
	}
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
		if w.timer != nil { // nil until an event creates it, or when start refused before any
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
