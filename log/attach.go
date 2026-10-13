package log

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"

	"go.uber.org/zap/zapcore"
)

// ── Attached sinks ─────────────────────────────────────────

// Attach registers core as an additional sink on the default backend and
// returns its detach function.
//
// The attached core is treated like a built-in sink: it is wrapped in its own
// maskCore layer with the same masker the console and file sinks use, it only
// receives entries the global level (Init's log.level or a later SetLevel)
// and its own Enabled admit, and it sits under the sampler, so a record
// sampling drops reaches no attached core either. Delivery is synchronous.
// The detach is idempotent; after it returns no new record is delivered to
// the core, while a delivery already in progress may still complete (detach
// does not wait for writers). Attach(nil) registers nothing and returns a
// no-op detach.
//
// The seam is a core inside the assembled backend, not a replacement of the
// global Logger (that is SetLogger's job), and that is what lets loggers
// derived before the attachment keep reaching it: an L().With(...) result --
// including the loggers plugins capture at construction -- cloned the core
// tree at derivation time, and the attach child it cloned resolves the
// current attachment set at write time, applying the fields its With calls
// had fixed.
//
// Attachments are process-global and independent of one backend generation: a
// later Init keeps them (re-wrapping each with that config's masker) and
// Close does not detach them, so a re-assembled backend restores delivery
// without the application re-attaching. Attach before the first Init
// registers the core, which begins receiving records once Init installs the
// backend. A backend installed via SetLogger carries no seam; records flow to
// attached cores only through this package's own backend.
func Attach(core zapcore.Core) (detach func()) {
	if core == nil {
		return func() {}
	}
	sink := attachmentState.attach(core)
	return func() { attachmentState.detach(sink) }
}

// attachedSink is one Attach registration: the core as the application
// supplied it, plus the masked view the fan-out delivers through. The masked
// view is rebuilt whenever Init installs a new masker, which is why both
// halves are kept.
type attachedSink struct {
	core    zapcore.Core
	wrapped zapcore.Core
}

// attachmentRegistry is the process-wide attachment set. Reads on the
// delivery path (Enabled/Check/Write/Sync) load the published snapshot
// lock-free; Attach, detach and Init's masker re-wrap mutate under mu and
// publish a fresh snapshot, so an in-flight delivery always sees either the
// old set or the new one, never a torn one.
type attachmentRegistry struct {
	mu     sync.Mutex
	masker *masker
	sinks  []*attachedSink
	active atomic.Pointer[[]zapcore.Core]

	// delivering tracks, per goroutine, whether an attached core is currently
	// being written on that goroutine. It is what tells a reentrant record
	// (an attached core logging through this package from inside its own
	// Write) from a concurrent one (a different goroutine, which must still
	// be delivered).
	gidMu      sync.Mutex
	delivering map[int64]struct{}
}

var attachmentState = attachmentRegistry{masker: newMasker(nil)}

func (r *attachmentRegistry) snapshot() []zapcore.Core {
	if p := r.active.Load(); p != nil {
		return *p
	}
	return nil
}

func (r *attachmentRegistry) anyEnabled(l zapcore.Level) bool {
	for _, core := range r.snapshot() {
		if core.Enabled(l) {
			return true
		}
	}
	return false
}

func (r *attachmentRegistry) attach(core zapcore.Core) *attachedSink {
	r.mu.Lock()
	defer r.mu.Unlock()
	sink := &attachedSink{core: core, wrapped: newMaskCore(core, r.masker)}
	r.sinks = append(r.sinks, sink)
	r.publishLocked()
	return sink
}

func (r *attachmentRegistry) detach(sink *attachedSink) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, s := range r.sinks {
		if s == sink {
			r.sinks = append(r.sinks[:i], r.sinks[i+1:]...)
			r.publishLocked()
			return
		}
	}
}

// installMasker points the registry at a new Init round's masker and re-wraps
// every attachment, so an attachment outlives a backend re-assembly without
// pinning the previous round's masking configuration.
func (r *attachmentRegistry) installMasker(m *masker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.masker = m
	for _, sink := range r.sinks {
		sink.wrapped = newMaskCore(sink.core, m)
	}
	r.publishLocked()
}

func (r *attachmentRegistry) publishLocked() {
	cores := make([]zapcore.Core, len(r.sinks))
	for i, sink := range r.sinks {
		cores[i] = sink.wrapped
	}
	r.active.Store(&cores)
}

// syncAll follows the global Sync: every attached core's Sync runs with the
// backend's own, and their errors are joined into the one Sync returns.
func (r *attachmentRegistry) syncAll() error {
	var errs []error
	for _, core := range r.snapshot() {
		if err := core.Sync(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *attachmentRegistry) enterDelivery() (int64, bool) {
	gid := goroutineID()
	r.gidMu.Lock()
	defer r.gidMu.Unlock()
	if _, busy := r.delivering[gid]; busy {
		return gid, false
	}
	if r.delivering == nil {
		r.delivering = make(map[int64]struct{})
	}
	r.delivering[gid] = struct{}{}
	return gid, true
}

func (r *attachmentRegistry) leaveDelivery(gid int64) {
	r.gidMu.Lock()
	delete(r.delivering, gid)
	r.gidMu.Unlock()
}

// goroutineID returns the calling goroutine's id, used solely to scope the
// reentrancy guard: a record logged by an attached core from inside its own
// Write must be suppressed for attached cores on that goroutine while
// concurrent records from other goroutines keep flowing. Go offers no
// goroutine-local storage, so the id is parsed from the runtime's stack
// header -- the dependency-free way every goroutine-id library uses. The cost
// is paid only after the lock-free check found attached cores, on a path
// dominated by the external sink's own write.
func goroutineID() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	const prefix = "goroutine "
	if n <= len(prefix) {
		return 0
	}
	var id int64
	for _, ch := range buf[len(prefix):n] {
		if ch < '0' || ch > '9' {
			break
		}
		id = id*10 + int64(ch-'0')
	}
	return id
}

// attachCore is the seam itself: a virtual sink in the assembled backend's
// Tee whose children are whatever is attached right now.
//
// The pending field is what defeats zap's usual With behavior in the seam's
// favor. zap's With fixes a derivation's fields by cloning the children's
// encoders, which is exactly what would make an attachment added later
// unreachable from loggers derived earlier. This core instead records the
// derivation's fields and re-applies them at write time, onto the attachment
// set as it stands then -- so a logger captured before Attach still delivers
// with its With fields baked in, without the attached core ever learning
// about derivation.
//
// The core is a Tee child, never wrapped in maskCore itself: it encodes
// nothing and each attached core carries its own maskCore layer, matching the
// per-sink masking order zap.go documents. It sits inside the sampler for the
// same reason the other sinks do.
//
// Deriving a logger through the seam costs two allocations on top of what
// zap's own Tee charges for any second child (multiCore.With allocates a
// fresh children slice and boxes it; a single-child Tee returns that child
// directly, which is why the pre-seam backend never paid it): the copy of the
// derivation's fields and the boxed core carrying them. The fields are copied
// rather than adopted because zap's With captures at derivation time: a
// caller that passes a slice it reuses and mutates afterwards -- legal
// through the Zap escape hatch, which hands its variadic slice straight to
// the core -- must not change what an already-derived logger delivers.
type attachCore struct {
	pending []zapcore.Field
}

func newAttachCore() zapcore.Core { return attachCore{} }

func (c attachCore) Enabled(l zapcore.Level) bool {
	// The snapshot load comes first on purpose: with nothing attached -- the
	// common case -- it short-circuits before the level enabler's atomic load
	// and before any per-attachment call, keeping the dead seam as cheap as
	// possible.
	p := attachmentState.active.Load()
	if p == nil || len(*p) == 0 || !levelState.level.Enabled(l) {
		return false
	}
	return attachmentState.anyEnabled(l)
}

func (c attachCore) With(fs []zapcore.Field) zapcore.Core {
	if len(fs) == 0 {
		return c
	}
	// Copied, never adopted: the caller may reuse the slice it passed (see
	// the type comment), and the delivery path re-reads pending at write
	// time, long after the call returned.
	pending := make([]zapcore.Field, 0, len(c.pending)+len(fs))
	pending = append(pending, c.pending...)
	pending = append(pending, fs...)
	return attachCore{pending: pending}
}

func (c attachCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	// The snapshot load is inlined here for the same reason it leads Enabled:
	// this is the method the assembled Tee calls on every record, and with
	// nothing attached a nil snapshot is the whole answer.
	if p := attachmentState.active.Load(); p == nil || len(*p) == 0 {
		return ce
	}
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

func (c attachCore) Write(ent zapcore.Entry, fs []zapcore.Field) error {
	cores := attachmentState.snapshot()
	if len(cores) == 0 {
		return nil
	}
	gid, ok := attachmentState.enterDelivery()
	if !ok {
		// A record logged on this goroutine by an attached core's own Write:
		// it still reaches console and file above, but sending it back into
		// the attached set would recurse or amplify.
		return nil
	}
	defer attachmentState.leaveDelivery(gid)
	fields := fs
	if len(c.pending) != 0 {
		fields = make([]zapcore.Field, 0, len(c.pending)+len(fs))
		fields = append(fields, c.pending...)
		fields = append(fields, fs...)
	}
	var errs []error
	for _, core := range cores {
		if !core.Enabled(ent.Level) {
			continue
		}
		if err := core.Write(ent, fields); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c attachCore) Sync() error { return attachmentState.syncAll() }
