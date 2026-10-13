package log

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// ── fixtures ───────────────────────────────────────────────

// recordingCore is an attached sink that records what it receives as parsed
// JSON, so tests can assert both field presence and masking.
type recordingCore struct {
	mu      sync.Mutex
	enabled zapcore.LevelEnabler
	prefix  []zapcore.Field
	entries []map[string]any
	raw     []string
	syncs   int

	// onWrite runs after each recorded write, inside the seam's delivery --
	// the hook the reentrancy test uses to log from inside an attached core.
	onWrite func(zapcore.Entry)
}

func newRecordingCore(enabled zapcore.LevelEnabler) *recordingCore {
	return &recordingCore{enabled: enabled}
}

func (c *recordingCore) Enabled(l zapcore.Level) bool { return c.enabled.Enabled(l) }

func (c *recordingCore) With(fs []zapcore.Field) zapcore.Core {
	prefix := make([]zapcore.Field, 0, len(c.prefix)+len(fs))
	prefix = append(prefix, c.prefix...)
	prefix = append(prefix, fs...)
	return &recordingCore{enabled: c.enabled, prefix: prefix, onWrite: c.onWrite}
}

func (c *recordingCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

func (c *recordingCore) Write(ent zapcore.Entry, fs []zapcore.Field) error {
	fields := fs
	if len(c.prefix) != 0 {
		fields = make([]zapcore.Field, 0, len(c.prefix)+len(fs))
		fields = append(fields, c.prefix...)
		fields = append(fields, fs...)
	}
	enc := zapcore.NewJSONEncoder(zapcore.EncoderConfig{
		MessageKey: "msg",
		LevelKey:   "level", EncodeLevel: zapcore.LowercaseLevelEncoder,
		TimeKey: "ts", EncodeTime: zapcore.EpochTimeEncoder,
	})
	buf, err := enc.EncodeEntry(ent, fields)
	if err != nil {
		return err
	}
	line := strings.TrimSpace(buf.String())
	var decoded map[string]any
	if err := json.Unmarshal([]byte(line), &decoded); err != nil {
		return err
	}
	c.mu.Lock()
	c.entries = append(c.entries, decoded)
	c.raw = append(c.raw, line)
	hook := c.onWrite
	c.mu.Unlock()
	if hook != nil {
		hook(ent)
	}
	return nil
}

func (c *recordingCore) Sync() error {
	c.mu.Lock()
	c.syncs++
	c.mu.Unlock()
	return nil
}

func (c *recordingCore) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *recordingCore) recorded() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.entries...)
}

func (c *recordingCore) rawLines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.raw...)
}

func (c *recordingCore) syncCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.syncs
}

// resetAttachments returns the process-wide attachment set to empty, so one
// test's leftovers cannot reach the next.
func resetAttachments() {
	attachmentState.mu.Lock()
	defer attachmentState.mu.Unlock()
	attachmentState.sinks = nil
	attachmentState.publishLocked()
	attachmentState.gidMu.Lock()
	attachmentState.delivering = nil
	attachmentState.gidMu.Unlock()
}

// initAttachLogger assembles a backend with the console sink off and one JSON
// file sink on, and returns the file path. Every test using it leaves the
// package globals clean.
func initAttachLogger(t *testing.T, level string, maskFields ...string) string {
	t.Helper()
	resetAttachments()
	t.Cleanup(resetAttachments)
	path := filepath.Join(t.TempDir(), "app.jsonl")
	cfg := DefaultConfig()
	cfg.Level = level
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = path
	cfg.MaskFields = maskFields
	require.NoError(t, Init(cfg))
	t.Cleanup(func() {
		_ = Close()
		SetLogger(Nop())
	})
	return path
}

func readJSONLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	require.NoError(t, Sync(), "flush the file sink before reading it")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	var entries []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), "file sink line: %s", line)
		entries = append(entries, m)
	}
	return entries
}

func messagesOf(entries []map[string]any) []string {
	msgs := make([]string, 0, len(entries))
	for _, entry := range entries {
		msg, _ := entry["msg"].(string)
		msgs = append(msgs, msg)
	}
	return msgs
}

// ── behavior ───────────────────────────────────────────────

// TestAttachedCoreReceivesLoggersDerivedBeforeAttachment pins the seam's core
// promise: the attachment is added after loggers were derived, and those
// loggers' records still arrive with the fields their With calls fixed.
func TestAttachedCoreReceivesLoggersDerivedBeforeAttachment(t *testing.T) {
	path := initAttachLogger(t, "info")
	before := L().With("plugin", "orders", "instance", "a")
	rec := newRecordingCore(zapcore.DebugLevel)
	detach := Attach(rec)
	defer detach()

	before.Info("handled", "order_id", 7)
	L().Info("plain")
	L().With("late", "yes").Warn("warned")

	entries := rec.recorded()
	require.Len(t, entries, 3)
	assert.Equal(t, "handled", entries[0]["msg"])
	assert.Equal(t, "orders", entries[0]["plugin"], "a logger derived before Attach must deliver its With fields")
	assert.Equal(t, "a", entries[0]["instance"])
	assert.EqualValues(t, 7, entries[0]["order_id"])
	assert.Equal(t, "plain", entries[1]["msg"])
	assert.NotContains(t, entries[1], "plugin")
	assert.Equal(t, "warned", entries[2]["msg"])
	assert.Equal(t, "yes", entries[2]["late"])

	fileEntries := readJSONLines(t, path)
	require.Len(t, fileEntries, 3, "the file sink keeps receiving the same records")
	assert.Equal(t, "orders", fileEntries[0]["plugin"])
}

// TestAttachedCoreSeesMaskedFields pins that the attachment is masked like a
// built-in sink: both call-site and With fields pass through this package's
// masker before the attached core sees them.
func TestAttachedCoreSeesMaskedFields(t *testing.T) {
	initAttachLogger(t, "info", "salary")
	rec := newRecordingCore(zapcore.DebugLevel)
	detach := Attach(rec)
	defer detach()

	L().With("salary", 50000).Info("paid",
		"password", "hunter2-secret", "access_token", "tok-abc", "user", "alice")

	raw := strings.Join(rec.rawLines(), "\n")
	for _, secret := range []string{"hunter2-secret", "tok-abc", "50000"} {
		assert.NotContains(t, raw, secret, "attached core received an unmasked value")
	}
	entries := rec.recorded()
	require.Len(t, entries, 1)
	assert.Equal(t, maskPlaceholder, entries[0]["password"])
	assert.Equal(t, maskPlaceholder, entries[0]["salary"], "configured mask_fields apply to attached cores too")
	assert.Equal(t, "alice", entries[0]["user"])
}

// TestAttachedCoreFollowsTheGlobalLevel pins both gates: an attached core
// receives only records the global level admits -- including a level moved by
// SetLevel after version -- and only those its own Enabled accepts.
func TestAttachedCoreFollowsTheGlobalLevel(t *testing.T) {
	initAttachLogger(t, "info")
	all := newRecordingCore(zapcore.DebugLevel)
	warnOnly := newRecordingCore(zapcore.WarnLevel)
	detachAll := Attach(all)
	detachWarn := Attach(warnOnly)
	defer detachAll()
	defer detachWarn()

	L().Debug("below-global")
	assert.Zero(t, all.count(), "a record below the global level must not reach even a debug-enabled attachment")

	L().Info("at-global")
	L().Warn("warn-level")
	assert.Equal(t, 2, all.count())
	require.Equal(t, 1, warnOnly.count(), "the attached core's own Enabled must filter")
	assert.Equal(t, "warn-level", warnOnly.recorded()[0]["msg"])

	_, err := SetLevel("debug")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = SetLevel("info") })
	L().Debug("now-below")
	assert.Equal(t, 3, all.count(), "SetLevel must open the seam for debug records")
	assert.Equal(t, 1, warnOnly.count())
}

// TestDetachStopsDeliveryAndIsIdempotent pins the removal contract: after
// detach nothing new arrives at that core, a second detach is harmless, and
// other attachments keep receiving.
func TestDetachStopsDeliveryAndIsIdempotent(t *testing.T) {
	initAttachLogger(t, "info")
	first := newRecordingCore(zapcore.DebugLevel)
	second := newRecordingCore(zapcore.DebugLevel)
	detachFirst := Attach(first)
	detachSecond := Attach(second)
	defer detachSecond()

	L().Info("before")
	assert.Equal(t, 1, first.count())
	assert.Equal(t, 1, second.count())

	detachFirst()
	detachFirst() // idempotent: no panic, no double removal
	L().Info("after")
	assert.Equal(t, 1, first.count(), "a detached core must not receive records")
	assert.Equal(t, 2, second.count(), "detaching one core must not touch another")
}

// TestReentrantAttachedWriteDoesNotRecurse pins the reentrancy guard: an
// attached core logging through this package from inside its own Write is not
// delivered back into the attached set, while the record still reaches
// console and file.
func TestReentrantAttachedWriteDoesNotRecurse(t *testing.T) {
	path := initAttachLogger(t, "info")
	rec := newRecordingCore(zapcore.DebugLevel)
	// The hook logs at most once, tracked by a flag rather than sync.Once:
	// without the guard this test's mutant must fail on the count assertion,
	// not deadlock re-entering a Once already executing on this goroutine.
	var hookMu sync.Mutex
	logged := false
	rec.onWrite = func(zapcore.Entry) {
		hookMu.Lock()
		first := !logged
		logged = true
		hookMu.Unlock()
		if first {
			L().Warn("attached sink reentered")
		}
	}
	detach := Attach(rec)
	defer detach()

	L().Info("outer")

	entries := rec.recorded()
	require.Len(t, entries, 1, "the reentrant record must not be delivered to attached cores")
	assert.Equal(t, "outer", entries[0]["msg"])

	fileMsgs := messagesOf(readJSONLines(t, path))
	assert.Contains(t, fileMsgs, "outer")
	assert.Contains(t, fileMsgs, "attached sink reentered", "the reentrant record must still reach console/file")
}

// TestAllSinksDisabledStillDeliversToAttachedCores pins the degenerate
// assembly: with every built-in sink disabled the backend behaves like Nop
// until something is attached, and a core attached afterwards still receives
// records -- the order an application plugin attaching during Start produces.
func TestAllSinksDisabledStillDeliversToAttachedCores(t *testing.T) {
	resetAttachments()
	t.Cleanup(resetAttachments)
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = false
	require.NoError(t, Init(cfg))
	t.Cleanup(func() {
		_ = Close()
		SetLogger(Nop())
	})

	assert.False(t, L().Enabled(ErrorLevel), "with nothing attached the backend is equivalent to Nop")
	assert.NotPanics(t, func() { L().Error("dropped") })

	rec := newRecordingCore(zapcore.DebugLevel)
	detach := Attach(rec)
	defer detach()
	L().Info("to the attachment")
	require.Equal(t, 1, rec.count())
	assert.Equal(t, "to the attachment", rec.recorded()[0]["msg"])
}

// TestAttachmentsSurviveInitAndAdoptTheNewMasker pins the chosen Init
// re-entry semantics: the attachment set is kept across a re-assembly, each
// attachment is re-wrapped with the new config's masker, and the new backend
// delivers each record exactly once.
func TestAttachmentsSurviveInitAndAdoptTheNewMasker(t *testing.T) {
	initAttachLogger(t, "info")
	rec := newRecordingCore(zapcore.DebugLevel)
	detach := Attach(rec)
	defer detach()

	L().Info("first-round", "team", "payments")
	require.Equal(t, 1, rec.count())

	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = filepath.Join(t.TempDir(), "second.jsonl")
	cfg.MaskFields = []string{"trace_note"}
	require.NoError(t, Init(cfg))

	L().Info("second-round", "trace_note", "sensitive-note")
	require.Equal(t, 2, rec.count(), "the attachment must survive Init and deliver exactly once through the new backend")
	last := rec.recorded()[1]
	assert.Equal(t, "second-round", last["msg"])
	assert.Equal(t, maskPlaceholder, last["trace_note"], "a re-assembly must re-wrap the attachment with the new masker")
}

// TestGlobalSyncReachesAttachedCores pins that Sync follows the global: both
// Sync and Close flush attached cores alongside the built-in sinks.
func TestGlobalSyncReachesAttachedCores(t *testing.T) {
	initAttachLogger(t, "info")
	rec := newRecordingCore(zapcore.DebugLevel)
	detach := Attach(rec)
	defer detach()

	require.NoError(t, Sync())
	assert.Equal(t, 1, rec.syncCount(), "Sync must reach attached cores")
	require.NoError(t, Close())
	assert.Equal(t, 2, rec.syncCount(), "Close must flush attached cores before it swaps the backend out")
}

// TestConcurrentAttachDetachAndLogging exercises the seam under the race
// detector: writers keep logging while attachments come and go.
func TestConcurrentAttachDetachAndLogging(t *testing.T) {
	resetAttachments()
	t.Cleanup(resetAttachments)
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = false
	require.NoError(t, Init(cfg))
	t.Cleanup(func() {
		_ = Close()
		SetLogger(Nop())
	})

	stop := make(chan struct{})
	var writers sync.WaitGroup
	for range 4 {
		writers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				L().Info("concurrent", "writer", "yes")
			}
		})
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		detach := Attach(newRecordingCore(zapcore.DebugLevel))
		L().Info("attaching", "seam", "live")
		detach()
	}
	close(stop)
	writers.Wait()
}

// ── benchmark guards ───────────────────────────────────────

// recordPathLogger builds the facade over a single discarding sink resembling
// Init's assembly, optionally carrying the attach seam. with_seam is what
// Init now always appends; without_attach_child is the previous shape.
func recordPathLogger(withSeam bool) Logger {
	enc := zapcore.NewJSONEncoder(jsonEncoderConfig())
	discard := newMaskCore(
		zapcore.NewCore(enc, zapcore.AddSync(io.Discard), zapcore.DebugLevel),
		newMasker(nil),
	)
	cores := []zapcore.Core{discard}
	if withSeam {
		cores = append(cores, newAttachCore())
	}
	return newZapLogger(zap.New(zapcore.NewTee(cores...)))
}

// TestNoAttachmentPathMatchesBaselineAllocations is the automated half of the
// zero-overhead claim: with nothing attached, the record path allocates
// exactly what it allocated before the seam existed.
func TestNoAttachmentPathMatchesBaselineAllocations(t *testing.T) {
	resetAttachments()
	t.Cleanup(resetAttachments)
	baseline := recordPathLogger(false)
	seam := recordPathLogger(true)

	baselineAllocs := testing.AllocsPerRun(1000, func() { baseline.Info("record", "key", "value") })
	seamAllocs := testing.AllocsPerRun(1000, func() { seam.Info("record", "key", "value") })
	assert.Equal(t, baselineAllocs, seamAllocs,
		"the seam must not add allocations when nothing is attached")
}

// TestNoAttachmentWithPathAddsTheTeeSeamFloor pins the other half of the zero
// overhead claim: deriving a logger through an assembled backend costs at
// most four allocations more than the pre-seam single-sink backend did. Two
// of the four are zap's, not the seam's: a Tee of two or more children wraps
// them in multiCore, whose With allocates a fresh child slice and boxes it,
// while a single-child Tee returns that child directly. (Measured with a
// zero-size second child: the delta is exactly two, child implementation
// irrelevant.) The seam's own two are the copy of the derivation fields --
// zap's capture-on-derivation semantics require the copy, see the aliasing
// test below -- and the boxed core carrying them. The ceiling is asserted as
// an upper bound, not equality, so a future zap that makes its own path
// cheaper does not fail this test, while any allocation the seam adds beyond
// its floor does.
func TestNoAttachmentWithPathAddsTheTeeSeamFloor(t *testing.T) {
	resetAttachments()
	t.Cleanup(resetAttachments)
	baseline := recordPathLogger(false)
	seam := recordPathLogger(true)

	derive := func(l Logger) { _ = l.With("request_id", "01HY9Z6Q7V8W9X0Y1Z2A3B4C5D", "plugin", "kafka") }
	deriveAndLog := func(l Logger) {
		l.With("request_id", "01HY9Z6Q7V8W9X0Y1Z2A3B4C5D", "plugin", "kafka").Info("record", "key", "value")
	}
	baselineDerive := testing.AllocsPerRun(1000, func() { derive(baseline) })
	seamDerive := testing.AllocsPerRun(1000, func() { derive(seam) })
	assert.LessOrEqual(t, seamDerive, baselineDerive+4,
		"deriving a logger with nothing attached must stay inside the Tee seam's floor")
	baselineFull := testing.AllocsPerRun(1000, func() { deriveAndLog(baseline) })
	seamFull := testing.AllocsPerRun(1000, func() { deriveAndLog(seam) })
	assert.LessOrEqual(t, seamFull, baselineFull+4,
		"derive-and-log with nothing attached must stay inside the Tee seam's floor")
}

// TestZapDerivationCopiesFieldsSoLaterMutationsStayOutOfTheSeam pins that the
// seam captures a derivation's fields the way zap does, at derivation time.
// The Zap escape hatch hands the caller's variadic slice straight to the core
// -- unlike the facade's own With, which builds a fresh slice per call -- so a
// caller that reuses and mutates that slice must not change what an
// already-derived logger delivers. Without the copy, the attached core would
// read the mutated field at write time while the file sink kept the original
// value: one record, two sinks, two different fields.
func TestZapDerivationCopiesFieldsSoLaterMutationsStayOutOfTheSeam(t *testing.T) {
	path := initAttachLogger(t, "info")
	raw, ok := Zap(context.Background())
	require.True(t, ok, "the file-sink backend must expose the zap escape hatch")

	fields := []zapcore.Field{zap.String("tenant", "original")}
	derived := raw.With(fields...)
	fields[0] = zap.String("tenant", "MUTATED")

	rec := newRecordingCore(zapcore.DebugLevel)
	detach := Attach(rec)
	defer detach()
	derived.Info("after mutation")

	entries := rec.recorded()
	require.Len(t, entries, 1)
	assert.Equal(t, "original", entries[0]["tenant"],
		"a caller slice mutated after derivation must not reach an attached core")
	fileEntries := readJSONLines(t, path)
	require.Len(t, fileEntries, 1)
	assert.Equal(t, "original", fileEntries[0]["tenant"],
		"the attached core and the file sink must deliver the same fields")
}

func BenchmarkRecordPathNoAttachments(b *testing.B) {
	resetAttachments()
	for _, variant := range []struct {
		name string
		seam bool
	}{{"without_attach_child", false}, {"with_attach_child", true}} {
		l := recordPathLogger(variant.seam)
		b.Run(variant.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				l.Info("record", "key", "value")
			}
		})
	}
}

// BenchmarkWithPathNoAttachments reports the derivation half of the zero
// overhead claim, with allocations visible in the benchmark output.
func BenchmarkWithPathNoAttachments(b *testing.B) {
	resetAttachments()
	for _, variant := range []struct {
		name string
		seam bool
	}{{"without_attach_child", false}, {"with_attach_child", true}} {
		l := recordPathLogger(variant.seam)
		b.Run(variant.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				l.With("request_id", "01HY9Z6Q7V8W9X0Y1Z2A3B4C5D", "plugin", "kafka").Info("record", "key", "value")
			}
		})
	}
}

func BenchmarkAttachedDelivery(b *testing.B) {
	resetAttachments()
	l := newZapLogger(zap.New(zapcore.NewTee(
		newMaskCore(zapcore.NewCore(
			zapcore.NewJSONEncoder(jsonEncoderConfig()), zapcore.AddSync(io.Discard), zapcore.DebugLevel,
		), newMasker(nil)),
		newAttachCore(),
	)))
	detach := Attach(zapcore.NewCore(
		zapcore.NewJSONEncoder(jsonEncoderConfig()), zapcore.AddSync(io.Discard), zapcore.DebugLevel,
	))
	defer detach()

	b.ReportAllocs()
	for b.Loop() {
		l.Info("record", "key", "value")
	}
}
