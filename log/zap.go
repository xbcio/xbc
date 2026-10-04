package log

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

// badKeyName holds invalid KV arguments. Kept consistent with log/slog's
// convention.
const badKeyName = "!BADKEY"

type loggerKey struct{}

var (
	// atomic.Pointer instead of atomic.Value -- the latter requires the dynamic
	// type to be identical on every Store, and SetLogger swapping backends
	// would panic outright.
	global atomic.Pointer[Logger]

	// closerSlot holds the close function for the current sink; Init calls it
	// when the config is swapped, and Close calls it on shutdown.
	closerSlot atomic.Pointer[[]func() error]
)

// ── Facade implementation ─────────────────────────────────

// exitFunc is the process-termination hook Fatal goes through. It is a
// variable so tests can observe the exit instead of dying with the test
// binary; nothing outside this package may reassign it.
var exitFunc = os.Exit

// deferExitHook is the fatal hook installed on zapLogger.z. It does nothing,
// leaving termination to Fatal so the flush can happen first.
//
// It deliberately is NOT zapcore.WriteThenNoop: that constant is the option's
// zero value, so zap's terminalHookOverride rewrites it straight back to
// WriteThenFatal -- passing it would silently leave zap owning the exit, with
// the code reading as though it didn't.
type deferExitHook struct{}

func (deferExitHook) OnWrite(*zapcore.CheckedEntry, []zapcore.Field) {}

// zapLogger is the default binding.
//
// It holds two zap instances so the caller frame points to the right place:
//   - raw: without the facade's caller skip; Zap()'s escape hatch returns it,
//     so the caller is correct when a caller invokes raw.Info directly
//   - z: raw + AddCallerSkip(1); facade methods go through it, skipping the
//     zapLogger.Info frame itself
//
// z also carries the deferExitHook fatal hook so the facade's Fatal, not zap,
// decides when the process dies -- see Fatal. raw deliberately does not: the
// escape hatch hands back a plain *zap.Logger, and a caller reaching for it
// expects zap's documented semantics, exit included.
type zapLogger struct {
	raw *zap.Logger
	z   *zap.Logger

	once sync.Once
	next *zapLogger // lazily built skip+1 version, used by the T-series sugar
}

func newZapLogger(raw *zap.Logger) *zapLogger {
	return &zapLogger{
		raw: raw,
		z:   raw.WithOptions(zap.AddCallerSkip(1), zap.WithFatalHook(deferExitHook{})),
	}
}

func (l *zapLogger) Debug(msg string, kv ...any) { l.z.Debug(msg, toFields(kv)...) }
func (l *zapLogger) Info(msg string, kv ...any)  { l.z.Info(msg, toFields(kv)...) }
func (l *zapLogger) Warn(msg string, kv ...any)  { l.z.Warn(msg, toFields(kv)...) }
func (l *zapLogger) Error(msg string, kv ...any) { l.z.Error(msg, toFields(kv)...) }

// Fatal writes the entry, flushes, then exits -- strictly in that order.
//
// zap's default fatal hook calls os.Exit from inside the write. zapcore's own
// ioCore happens to Sync first for levels above Error, so with today's backend
// nothing is actually lost -- but that is one implementation's courtesy, not
// anything the zapcore.Core interface promises, and SetLogger lets a
// third-party core in that owes us nothing. Flushing here makes it the
// facade's guarantee instead of a coincidence.
func (l *zapLogger) Fatal(msg string, kv ...any) {
	l.z.Fatal(msg, toFields(kv)...)
	if err := l.raw.Sync(); err != nil && !isBenignSyncError(err) {
		// The process is about to die, so stderr is the only channel left --
		// silently dropping this would hide a lost final entry.
		fmt.Fprintf(os.Stderr, "log: failed to flush Fatal entry to disk: %v\n", err)
	}
	exitFunc(1)
}

func (l *zapLogger) With(kv ...any) Logger {
	if len(kv) == 0 {
		return l
	}
	return newZapLogger(l.raw.With(toFields(kv)...))
}

func (l *zapLogger) Enabled(lv Level) bool {
	return l.raw.Core().Enabled(zapcore.Level(lv))
}

// Zap implements ZapProvider. Returns the instance without the facade's
// caller skip.
//
// It keeps zap's native fatal behaviour: Zap().Fatal exits from inside the
// write without the facade's flush. Use log.Fatal when that flush matters.
func (l *zapLogger) Zap() *zap.Logger { return l.raw }

// WithCallerSkip implements CallerSkipper.
func (l *zapLogger) WithCallerSkip(n int) Logger {
	if n != 1 {
		return newZapLogger(l.raw.WithOptions(zap.AddCallerSkip(n)))
	}
	// skip+1 is the T-series' hot path; cache it so we don't clone the logger
	// on every call.
	l.once.Do(func() {
		l.next = &zapLogger{
			raw: l.raw.WithOptions(zap.AddCallerSkip(1)),
			z:   l.z.WithOptions(zap.AddCallerSkip(1)),
		}
	})
	return l.next
}

// toFields converts a KV sequence into zap.Field values.
//
// Produces a !BADKEY field when the key isn't a string or an argument is
// left dangling, without panicking or silently swallowing it.
// A call migrated from gfa's Sprintln semantics (TInfo(ctx, "payment", id, amt),
// where id is an int) turns into two !BADKEY fields here, immediately
// visible.
func toFields(kv []any) []zap.Field {
	if len(kv) == 0 {
		return nil
	}
	fs := make([]zap.Field, 0, (len(kv)+1)/2)
	for i := 0; i < len(kv); {
		if i == len(kv)-1 { // the last, dangling item
			fs = append(fs, zap.Any(badKeyName, kv[i]))
			break
		}
		k, ok := kv[i].(string)
		if !ok { // the key position isn't a string: record it separately, retry the next item as a key
			fs = append(fs, zap.Any(badKeyName, kv[i]))
			i++
			continue
		}
		fs = append(fs, zap.Any(k, kv[i+1]))
		i += 2
	}
	return fs
}

// ── Global binding ─────────────────────────────────────────

// L returns the global Logger. Before Init, returns Nop, so logging never
// panics.
func L() Logger {
	if p := global.Load(); p != nil {
		return *p
	}
	return Nop()
}

// SetLogger replaces the global backend, corresponding to an SLF4J binding
// swap.
//
// Security note: replacing the default backend also replaces the built-in
// sensitive-field masking -- that's implemented in this package's maskCore,
// and a third-party implementation won't automatically carry it over. This
// prints a warning to stderr on every non-default replacement, deliberately
// not gated by sync.Once: each call to SetLogger is an independent security
// event (the new backend may or may not carry its own masking), so every
// replacement deserves its own explicit warning. Silencing the second
// warning with sync.Once would let a second replacement go unnoticed, which
// is a security regression, not a UX improvement.
func SetLogger(l Logger) {
	if l == nil {
		l = Nop()
	}
	global.Store(&l)

	switch l.(type) {
	case *zapLogger, nopLogger:
		// The default binding, or explicitly disabling logging, needs no warning
	default:
		fmt.Fprintln(os.Stderr,
			"xbc/log: default logging backend has been replaced, built-in sensitive field masking is therefore disabled — "+
				"please ensure the new backend implements masking itself, otherwise password/token fields will be logged in plain text")
	}
}

// NewContext binds a Logger into ctx.
func NewContext(ctx context.Context, l Logger) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, loggerKey{}, l)
}

// Ctx retrieves the Logger stored on ctx.
//
// Fast path: entry middleware uses NewContext to store a logger already
// carrying trace fields; this retrieves it directly with zero allocations.
// Slow path: when only a Trace is present with no Logger, one is derived on
// the spot -- every call allocates, so the framework's entry middleware
// should always go through NewContext.
// If neither is present, falls back to the global Logger without panicking.
func Ctx(ctx context.Context) Logger {
	if ctx == nil {
		return L()
	}
	if l, ok := ctx.Value(loggerKey{}).(Logger); ok && l != nil {
		return l
	}
	if t := TraceFrom(ctx); t.Valid() {
		return L().With(traceKV(t)...)
	}
	return L()
}

// Zap returns the underlying *zap.Logger for zap-specific operations. ok is
// false when the backend isn't zap.
//
// The escape hatch is protected by masking too -- maskCore is part of this
// logger's composition and cannot be bypassed.
func Zap(ctx context.Context) (*zap.Logger, bool) {
	if zp, ok := Ctx(ctx).(ZapProvider); ok {
		return zp.Zap(), true
	}
	return nil, false
}

// ── Assembly ───────────────────────────────────────────────

// Init assembles the default zap backend from the config and sets it as
// global. Calling it repeatedly closes the previously opened file sink first.
func Init(cfg Config) error {
	if err := cfg.Normalize(); err != nil {
		return err
	}
	lv, err := ParseLevel(cfg.Level)
	if err != nil {
		return err
	}

	var (
		cores []zapcore.Core
		cls   []func() error
	)

	// The three sinks share the same masker, each wrapped in its own maskCore
	// layer (see the NewTee call below for why).
	m := newMasker(cfg.MaskFields)

	if cfg.Console.Enabled {
		cores = append(cores, newMaskCore(zapcore.NewCore(
			buildEncoder(cfg.Console.Format, wantColor(cfg.Console.Color, os.Stdout)),
			zapcore.Lock(os.Stdout),
			zapcore.Level(lv),
		), m))
	}

	if cfg.File.Enabled {
		w, closeFn, err := buildFileWriter(cfg.File, cfg.File.Path)
		if err != nil {
			closeAll(cls)
			return err
		}
		cls = append(cls, closeFn)
		cores = append(cores, newMaskCore(zapcore.NewCore(
			buildEncoder(cfg.File.Format, false), w, zapcore.Level(lv)), m))

		if cfg.File.ErrorPath != "" {
			ew, ecloseFn, err := buildFileWriter(cfg.File, cfg.File.ErrorPath)
			if err != nil {
				closeAll(cls)
				return err
			}
			cls = append(cls, ecloseFn)
			cores = append(cores, newMaskCore(zapcore.NewCore(
				buildEncoder(cfg.File.errorFormat, false), ew, zapcore.ErrorLevel), m))
		}
	}

	if len(cores) == 0 { // everything off is equivalent to Nop, common in test environments
		SetLogger(Nop()) // swap the global first, then close -- see the swap/close ordering note below
		closeAll(swapClosers(nil))
		return nil
	}

	// Masking has already been wrapped per sink; this only fans out.
	//
	// Never reverse this by wrapping maskCore outside the Tee: maskCore.Check
	// would hang itself onto the CheckedEntry, so multiCore.Check
	// (zapcore/tee.go:74-79 -- the only place per-sink filtering happens)
	// would never run, and multiCore.Write would then unconditionally write
	// into every child core, turning error_path into a complete copy of
	// app.log.
	core := zapcore.NewTee(cores...)

	// Sampling must be at the outermost layer, wrapped outside maskCore --
	// sampler.Check (zapcore/sampler.go:214-229) is the only place sampling
	// happens; if it were short-circuited by maskCore.Check, log.sampling
	// would silently stop working. Side benefit: dropped logs also skip the
	// masking overhead.
	if cfg.Sampling.Initial > 0 {
		core = zapcore.NewSamplerWithOptions(core, time.Second,
			cfg.Sampling.Initial, cfg.Sampling.Thereafter)
	}

	opts := []zap.Option{zap.ErrorOutput(zapcore.Lock(os.Stderr))}
	if cfg.Caller {
		opts = append(opts, zap.AddCaller())
	}
	if st, err := ParseLevel(cfg.Stacktrace); err == nil {
		opts = append(opts, zap.AddStacktrace(zapcore.Level(st)))
	}

	// Swap the global logger to the new backend FIRST, then close the
	// previous round's sinks. Reversed, there is a window after closeAll
	// and before SetLogger where a concurrent caller using L() still gets
	// the old *zapLogger, whose sinks were just closed -- lumberjack would
	// silently reopen the file it was just told to close (see
	// lazyClosingSink's doc). Swapping first means any write that still
	// lands on the old logger during the handover hits the old sink while
	// it is still legitimately open; only after the swap do we close it,
	// at which point lazyClosingSink makes further writes to it an error
	// rather than a silent reopen.
	SetLogger(newZapLogger(zap.New(core, opts...)))
	closeAll(swapClosers(cls))
	return nil
}

func buildEncoder(format string, color bool) zapcore.Encoder {
	if format == FormatJSON {
		return zapcore.NewJSONEncoder(jsonEncoderConfig())
	}
	return newConsoleEncoder(color)
}

func jsonEncoderConfig() zapcore.EncoderConfig {
	c := zap.NewProductionEncoderConfig()
	c.TimeKey = "ts"
	c.EncodeTime = zapcore.RFC3339NanoTimeEncoder
	c.LevelKey = "level"
	c.EncodeLevel = zapcore.LowercaseLevelEncoder
	c.MessageKey = "msg"
	c.CallerKey = "caller"
	c.EncodeCaller = zapcore.ShortCallerEncoder
	c.StacktraceKey = "stack"
	c.EncodeDuration = zapcore.MillisDurationEncoder
	return c
}

// lazyClosingSink wraps a file-backed zapcore.WriteSyncer so that once its
// close func has run, Write becomes a no-op that reports an error instead of
// forwarding to the underlying sink.
//
// This exists because of lumberjack's reopen-on-write behavior: lumberjack's
// own Write checks `if l.file == nil { openExistingOrNew }`, with no
// awareness of an explicit Close -- so a caller holding an earlier log.L()
// result that keeps writing after Init's swap (or after Close) would
// silently recreate the file lumberjack just closed, line by line, forever.
// Returning an error here instead routes the write through zap's
// ErrorOutput (zapcore/entry.go's CheckedEntry.Write), which Init wires to
// stderr -- so the caller gets a visible signal instead of a resurrected
// file.
//
// A reader holds mu's read lock across both the closed check and the
// delegate, so a close cannot land in between them -- checking first and then
// delegating would leave exactly the window this type exists to remove.
// Close takes the write lock only to flip the flag, and runs the real close
// func after releasing it: any Write that passed the check has already
// finished delegating by then, and every later Write sees closed and is
// discarded without touching the sink. That ordering also keeps Close from
// holding a lock while close() runs, which matters because lumberjack.Close
// takes lumberjack's own lock and a write racing it would otherwise be made
// to wait on cleanup it has nothing to do with.
type lazyClosingSink struct {
	zapcore.WriteSyncer
	mu     sync.RWMutex
	closed bool
	close  func() error
}

// errSinkClosed is what a write to a closed sink reports. It is a package
// value rather than a fmt.Errorf per call because the discard path is reached
// by every write from a logger that outlived its sink, and allocating a fresh
// error for each one is pure waste.
var errSinkClosed = errors.New("log: write to a closed sink was discarded")

func newLazyClosingSink(w zapcore.WriteSyncer, close func() error) *lazyClosingSink {
	return &lazyClosingSink{WriteSyncer: w, close: close}
}

func (s *lazyClosingSink) Write(p []byte) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return 0, errSinkClosed
	}
	return s.WriteSyncer.Write(p)
}

func (s *lazyClosingSink) Sync() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil
	}
	return s.WriteSyncer.Sync()
}

// Close marks the sink closed, so every subsequent Write is discarded
// instead of being delegated to the (now closed) underlying sink, then runs
// the real close func. Safe to call more than once; only the first call
// actually closes.
func (s *lazyClosingSink) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	if s.close == nil {
		return nil
	}
	return s.close()
}

func buildFileWriter(cfg FileConfig, path string) (zapcore.WriteSyncer, func() error, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		// 0o750 instead of 0o755: the log directory shouldn't be readable by
		// other users.
		//
		// This MkdirAll intentionally duplicates the one inside
		// newDailyRotator: under the RotateSize path, there is no
		// newDailyRotator call at all, so this is the only place the
		// directory gets created. Under the RotateDaily path both
		// MkdirAll calls execute, but that is harmless — MkdirAll on an
		// existing directory returns nil without touching permissions
		// (measured), so the second call is a no-op.
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, nil, fmt.Errorf("log: failed to create log directory %s: %w", dir, err)
		}
	}
	lj := &lumberjack.Logger{
		Filename:   path,
		MaxSize:    cfg.MaxSize,
		MaxAge:     cfg.MaxAge,
		MaxBackups: cfg.MaxBackups,
		Compress:   cfg.Compress,
		LocalTime:  true,
	}
	// Wrapped in lazyClosingSink regardless of rotation mode: once closeFn
	// has run, further writes must be discarded with an error rather than
	// being delegated to lumberjack (which would silently reopen the file
	// it was just closed from under -- see lazyClosingSink's own doc).
	if cfg.Rotate == RotateDaily {
		d := newDailyRotator(lj)
		sink := newLazyClosingSink(zapcore.AddSync(d), d.Close)
		return sink, sink.Close, nil
	}
	sink := newLazyClosingSink(zapcore.AddSync(lj), lj.Close)
	return sink, sink.Close, nil
}

func swapClosers(next []func() error) []func() error {
	old := closerSlot.Swap(&next)
	if old == nil {
		return nil
	}
	return *old
}

func closeAll(fns []func() error) {
	for _, fn := range fns {
		if fn != nil {
			_ = fn()
		}
	}
}

// Sync flushes to disk. Call it before the process exits, typically via
// defer.
func Sync() error {
	zp, ok := L().(ZapProvider)
	if !ok {
		return nil
	}
	if err := zp.Zap().Sync(); err != nil && !isBenignSyncError(err) {
		return err
	}
	return nil
}

// Close flushes to disk and closes all file sinks. The framework calls this
// as the last step of a graceful shutdown.
//
// After Close, the global logger -- if it is still this package's own
// *zapLogger -- is swapped to Nop(), so the facade records to nothing
// instead of silently falling through to a sink that lazyClosingSink has
// made error-returning. A third-party backend wired in later via SetLogger
// is left untouched: this package did not open its sinks and has no
// business tearing it down.
//
// Sync runs before the swap/close for the same reason Init swaps before
// closing: a concurrent writer still holding the old logger during the
// handover should hit a sink that is flushed and only then closed, not a
// half-torn-down one.
func Close() error {
	err := Sync()

	// Load-then-conditional-SetLogger rather than an unconditional
	// SetLogger(Nop()): a concurrent SetLogger(thirdParty) call interleaved
	// with this check is an inherent TOCTOU on a global the whole package
	// treats as externally swappable at any time (SetLogger itself offers
	// no stronger guarantee) -- the goal here is only to not clobber a
	// third-party backend that was already in place when Close started.
	if p := global.Load(); p != nil {
		if _, ok := (*p).(*zapLogger); ok {
			SetLogger(Nop())
		}
	}

	closeAll(swapClosers(nil))
	return err
}

// isBenignSyncError recognizes the normal failure of calling Sync on
// stdout/stderr. Terminals and pipes aren't fsync-able objects, so the kernel
// returns EINVAL/ENOTTY -- this isn't a real error, just a well-known zap
// wart, and shouldn't make a deferred log.Sync() report an error on every
// exit.
func isBenignSyncError(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, syscall.EINVAL) ||
		errors.Is(err, syscall.ENOTTY) ||
		errors.Is(err, syscall.EBADF) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "invalid argument") ||
		strings.Contains(s, "inappropriate ioctl") ||
		strings.Contains(s, "bad file descriptor")
}
