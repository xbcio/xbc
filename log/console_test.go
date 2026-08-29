package log

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func encodeOne(t *testing.T, color bool, ent zapcore.Entry, with []zapcore.Field, fs []zapcore.Field) string {
	t.Helper()
	enc := newConsoleEncoder(color)
	for _, f := range with {
		f.AddTo(enc)
	}
	buf, err := enc.EncodeEntry(ent, fs)
	require.NoError(t, err)
	return buf.String()
}

func sampleEntry() zapcore.Entry {
	return zapcore.Entry{
		Level:   zapcore.InfoLevel,
		Time:    time.Date(2026, 8, 24, 10, 23, 45, 123_000_000, time.Local),
		Message: "Validation passed",
		Caller: zapcore.EntryCaller{
			Defined: true,
			File:    "/home/u/proj/order/service.go",
			Line:    42,
		},
	}
}

func TestConsoleLayout(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.String("trace_id", "01926f7e1a2b3c4d5e6f708192a3b4c5"),
		zap.Int("order_id", 1001),
		zap.Int("amount", 99),
	})

	assert.True(t, strings.HasSuffix(out, "\n"), "Must end with newline")
	line := strings.TrimSuffix(out, "\n")

	assert.True(t, strings.HasPrefix(line, "10:23:45.123 "), "Time range: %q", line)
	assert.Contains(t, line, "INFO  ", "level left-aligned padded to 6 width (widthLevel)")
	assert.Contains(t, line, "01926f7e", "trace column takes first 8 digits of trace_id")
	assert.NotContains(t, line, "01926f7e1a2b", "console does not print full trace_id")
	assert.Contains(t, line, "order/service.go:42")
	assert.Contains(t, line, "Validation passed")
	assert.Contains(t, line, "amount=99")
	assert.Contains(t, line, "order_id=1001")
	assert.NotContains(t, line, "trace_id=", "trace_id occupies fixed column, no longer enters KV area")
}

func TestConsoleFieldsSortedByKey(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.Int("zebra", 1), zap.Int("apple", 2), zap.Int("mango", 3),
	})
	assert.Less(t, strings.Index(out, "apple="), strings.Index(out, "mango="))
	assert.Less(t, strings.Index(out, "mango="), strings.Index(out, "zebra="))
}

func TestConsoleWithFieldsComeBeforeCallFields(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(),
		[]zapcore.Field{zap.String("service", "order")},
		[]zapcore.Field{zap.Int("amount", 99)},
	)
	assert.Less(t, strings.Index(out, "service=order"), strings.Index(out, "amount=99"),
		"With context fields appear before current call fields")
}

// Note: an overly long TrimmedPath cannot be produced by adding more path
// depth -- TrimmedPath only keeps the last two path segments, and a deeper
// path often produces a shorter result on TrimmedPath (the parent dir name
// can be short). To trigger truncation, "parent dir name + file name + line
// number" itself must be long enough, e.g. by making both the package name
// and file name long: TrimmedPath comes out as
// "verylongpackagename/handlerimplementation.go:1234", 49 characters,
// comfortably over widthCaller(24).
func TestConsoleCallerTruncatedFromLeft(t *testing.T) {
	ent := sampleEntry()
	ent.Caller.File = "/src/verylongpackagename/handlerimplementation.go"
	ent.Caller.Line = 1234

	out := encodeOne(t, false, ent, nil, nil)
	assert.Contains(t, out, "…", "Long caller truncates from the left")
	assert.Contains(t, out, ".go:1234", "Line number side must be fully preserved")
}

// Asserts that the caller segment's actual rendered width equals exactly
// widthCaller -- only asserting "how many leading spaces" would not pin
// down the actual column width; if someone changes widthCaller, this test
// still wouldn't fail.
//
// Note: the assertions below computed off the widthCaller symbol itself
// (things like len(seg) == widthCaller) do not pin down "widthCaller should
// be 24" itself -- once the constant changes, these assertions reference
// the same constant and change along with it, so they will never fail.
// What actually pins down the concrete value 24 is the last line,
// assert.Equal(t, 24, widthCaller), plus TestConsoleCallerAtSpecWidthNotTruncated
// below, which does not reference the widthCaller symbol and instead
// asserts directly against the spec §8.7 example line.
func TestConsoleShortCallerRightAligned(t *testing.T) {
	ent := sampleEntry()
	ent.Caller.File = "/x/a/b.go"
	ent.Caller.Line = 7

	out := encodeOne(t, false, ent, nil, nil)
	// time(12) + space(1) + level(widthLevel) + space(1) + trace(widthTrace) + space(1)
	// is where the caller segment starts; the caller segment itself is widthCaller wide.
	callerStart := len("10:23:45.123") + 1 + widthLevel + 1 + widthTrace + 1
	seg := out[callerStart : callerStart+widthCaller]
	assert.Equal(t, widthCaller, len(seg), "caller segment must be exactly widthCaller wide")
	assert.Equal(t, "a/b.go:7", strings.TrimLeft(seg, " "), "Content of short caller")
	assert.Equal(t, strings.Repeat(" ", widthCaller-len("a/b.go:7"))+"a/b.go:7", seg,
		"Short caller right-aligned padded with spaces, width must exactly equal widthCaller")
	assert.Equal(t, 24, widthCaller, "widthCaller is based on spec §8.7 table, changes require re-review")
}

// The spec §8.7 table's example line "payment/client.go:33" is exactly 20
// characters, sitting right between 24 (no truncation) and the previously,
// mistakenly used 19 (would truncate) -- pinning that boundary down is this
// test's entire reason for existing: if anyone changes widthCaller back to
// 19 or smaller, this 20-character spec example line would get truncated
// into a "…", and this test must be able to catch that.
// It does not reference the widthCaller symbol, and instead asserts
// directly against the known concrete value, avoiding the self-proving trap
// of "change the constant, the assertion changes along with it".
func TestConsoleCallerAtSpecWidthNotTruncated(t *testing.T) {
	ent := sampleEntry()
	ent.Caller.File = "/src/payment/client.go"
	ent.Caller.Line = 33

	out := encodeOne(t, false, ent, nil, nil)
	assert.Contains(t, out, "    payment/client.go:33", "20-character spec example line right-aligned padded with 4 spaces, no truncation")
	assert.NotContains(t, out, "…", "If widthCaller is reverted to 19, this 20-character path will be truncated")
}

func TestConsoleNoCallerWhenUndefined(t *testing.T) {
	ent := sampleEntry()
	ent.Caller = zapcore.EntryCaller{}
	out := encodeOne(t, false, ent, nil, nil)
	assert.Contains(t, out, "Validation passed")
	assert.NotContains(t, out, "undefined")
}

func TestConsoleQuotesValuesWithSpaces(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.Error(errors.New("connection refused")),
		zap.String("plain", "ok"),
		zap.String("empty", ""),
	})
	assert.Contains(t, out, `error="connection refused"`)
	assert.Contains(t, out, "plain=ok", "Values without space do not need quotes")
	assert.Contains(t, out, `empty=""`)
}

// Non-ASCII text must remain literal rather than being escaped as \uXXXX.
func TestConsoleKeepsUTF8Literal(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.String("reason", "café balance"),
	})
	assert.Contains(t, out, `reason="café balance"`)
	assert.NotContains(t, out, `\u`)
}

func TestConsoleEscapesQuotesAndNewlines(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.String("raw", "he said \"hi\"\nbye"),
	})
	assert.Contains(t, out, `raw="he said \"hi\"\nbye"`)
	assert.Equal(t, 1, strings.Count(out, "\n"), "Newlines in values must be escaped, one log line per line")
}

// writeConsoleValue's backslash-escaping branch previously had no test
// guarding it: delete that case, and a backslash in a value would pass
// through unescaped inside the quotes, breaking the literal meaning
// relied on for grep/copy-paste.
func TestConsoleEscapesBackslash(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.String("path", `C:\temp file`),
	})
	assert.Contains(t, out, `path="C:\\temp file"`, "Backslash must be escaped into two characters")
}

func TestConsoleColoring(t *testing.T) {
	out := encodeOne(t, true, sampleEntry(), nil, []zapcore.Field{
		zap.Error(errors.New("boom")),
	})
	assert.Contains(t, out, ansiGreen+"INFO  "+ansiReset, "INFO in green")
	assert.Contains(t, out, ansiRed, "err field value in red")
	assert.Contains(t, out, ansiCyan, "key in cyan")
	assert.Contains(t, out, ansiDim, "Time/trace/caller in dark gray")
}

func TestConsoleLevelColors(t *testing.T) {
	cases := map[zapcore.Level]string{
		zapcore.DebugLevel:  ansiCyan,
		zapcore.InfoLevel:   ansiGreen,
		zapcore.WarnLevel:   ansiYellow,
		zapcore.ErrorLevel:  ansiRed,
		zapcore.DPanicLevel: ansiRed,
	}
	for lv, want := range cases {
		ent := sampleEntry()
		ent.Level = lv
		out := encodeOne(t, true, ent, nil, nil)
		assert.Contains(t, out, want+padRightRef(lv.CapitalString(), widthLevel)+ansiReset, "Level %s", lv)
	}
}

// Each level's level segment must occupy exactly widthLevel columns --
// DPANIC is 6 characters, the longest of all levels; if widthLevel were
// smaller than that, this line would shift entirely to the right.
func TestConsoleLevelColumnWidthIsUniform(t *testing.T) {
	levels := []zapcore.Level{
		zapcore.DebugLevel, zapcore.InfoLevel, zapcore.WarnLevel,
		zapcore.ErrorLevel, zapcore.DPanicLevel, zapcore.PanicLevel, zapcore.FatalLevel,
	}
	for _, lv := range levels {
		ent := sampleEntry()
		ent.Level = lv
		out := encodeOne(t, false, ent, nil, nil)
		// time segment fixed at 12 wide + 1 space, followed by widthLevel
		// wide, i.e. the level segment
		seg := out[13 : 13+widthLevel]
		assert.Equal(t, lv.CapitalString(), strings.TrimRight(seg, " "), "Text of level %s", lv)
		assert.Equal(t, widthLevel, len(seg), "Width of level %s column", lv)
		assert.Equal(t, byte(' '), out[13+widthLevel], "After level %s, must be immediately followed by separator space", lv)
	}
}

// Color codes have zero width: with and without coloring, the output must
// be byte-for-byte identical once ANSI is stripped.
func TestConsoleColorDoesNotBreakAlignment(t *testing.T) {
	ent := sampleEntry()
	fs := []zapcore.Field{zap.Int("n", 1)}
	plain := encodeOne(t, false, ent, nil, fs)
	colored := stripANSI(encodeOne(t, true, ent, nil, fs))
	assert.Equal(t, plain, colored)
}

// Fields after zap.Namespace get collected by MapObjectEncoder into a
// nested map, leaving only the namespace name at the top level. console
// must flatten this into a dotted full path, not print Go's map[k:v]
// literal.
func TestConsoleFlattensNamespace(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.String("svc", "order"),
		zap.Namespace("db"),
		zap.String("host", "10.0.0.1"),
		zap.Int("port", 5432),
	})
	assert.Contains(t, out, "db.host=10.0.0.1")
	assert.Contains(t, out, "db.port=5432")
	assert.Contains(t, out, "svc=order")
	assert.NotContains(t, out, "map[", "Cannot fall into Go's map literal")
}

// After flattening, consoleHiddenFields uses the full path for the check:
// the top-level trace_id is excluded as before, while a same-named field
// inside a namespace was explicitly placed there by the caller, and is
// kept.
func TestConsoleHiddenFieldsUseFullPath(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.String("trace_id", "0192abcd0192abcd"),
		zap.Namespace("upstream"),
		zap.String("trace_id", "ffffffffffffffff"),
	})
	assert.NotContains(t, out, "trace_id=0192abcd0192abcd", "Top-level trace_id occupies fixed column, no entry into KV area")
	assert.Contains(t, out, "upstream.trace_id=ffffffffffffffff")
}

// A self-referential map must not let the flattening recursion fail to
// terminate.
//
// The previous version only asserted "the recursion stopped" (m.k=v
// appeared) -- deleting the two depth-limit lines still left every test
// green, because even without the depth limit, "m.k=v" would still be
// produced at the very first level, and whether it keeps recursing
// infinitely afterward is invisible to that assertion. This adds two hard
// assertions: that the placeholder does appear once maxConsoleDepth is
// hit, and that it does not recurse one level further.
func TestConsoleNestedDepthIsBounded(t *testing.T) {
	m := map[string]any{"k": "v"}
	m["self"] = m
	done := make(chan string, 1)
	go func() { done <- encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{zap.Any("m", m)}) }()
	select {
	case out := <-done:
		assert.Contains(t, out, "m.k=v")

		// Depth limit hit at level 8: the full key is "m" followed by 8
		// ".self" segments, and the value must be the placeholder, not a
		// further-expanded sub-structure.
		//
		// The expected string is a literal, not computed from
		// maxConsoleDepth -- if someone changes the constant, this test
		// must actually turn red rather than self-adjusting along with it.
		atLimit := "m.self.self.self.self.self.self.self.self"
		assert.Contains(t, out, atLimit+"=<depth-limit>",
			"After reaching limit, must print placeholder, not silently discard or continue expanding")

		// Must not go one level deeper than this -- one more ".self" would
		// mean the depth limit did not take effect.
		assert.NotContains(t, out, atLimit+".self",
			"Cannot expand beyond 8 layers")
	case <-time.After(5 * time.Second):
		t.Fatal("Flattening recursion without stopping")
	}
}

// zap.ByteString is already a string by the time it lands in
// MapObjectEncoder (AddByteString does string(v) internally), so it gets
// caught by stringify's case string branch and never reaches the []byte
// branch. What this test actually asserts is stringify's handling of the
// string path, not the []byte branch.
func TestConsoleRendersByteStringAsText(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.ByteString("body", []byte("hi")),
	})
	assert.Contains(t, out, "body=hi")
	assert.NotContains(t, out, "[104 105]")
}

// stringify's []byte branch is actually entered via zap.Binary (AddBinary
// stores the value as-is as []byte, unlike AddByteString which converts to
// string first). Without special-casing it, this would print "[104 105]"
// instead of "hi".
func TestConsoleRendersBinaryAsText(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.Binary("body", []byte("hi")),
	})
	assert.Contains(t, out, "body=hi")
	assert.NotContains(t, out, "[104 105]")
}

// A caller path containing multibyte UTF-8 must not get sliced into
// U+FFFD, nor misaligned from counting by byte.
func TestConsoleCallerHandlesMultibyte(t *testing.T) {
	ent := sampleEntry()
	ent.Caller = zapcore.EntryCaller{
		Defined: true,
		File:    "/src/éééééééé/service/orders.go",
		Line:    42,
	}
	out := encodeOne(t, false, ent, nil, nil)
	assert.True(t, utf8.ValidString(out), "Output must be valid UTF-8")
	assert.NotContains(t, out, "�", "Cannot slice out replacement character")
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			i = j + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// Fix 2 regression: zapcore.ioCore.With is implemented as "Clone an
// encoder, then AddTo the fields into it". When zap.Namespace("db") and
// the following fields are split across two With calls, Clone must make
// sure the second call's fields still land in db, instead of falling back
// to the top level.
// This deliberately goes through the real zap.New(...).With(...).With(...)
// path, rather than constructing a nested map directly, because the
// triggering condition is precisely "two independent Clone calls".
func TestConsoleNamespacePreservedAcrossWith(t *testing.T) {
	var buf bytes.Buffer
	enc := newConsoleEncoder(false)
	core := zapcore.NewCore(enc, zapcore.AddSync(&buf), zapcore.DebugLevel)
	l := zap.New(core).
		With(zap.Namespace("db")).
		With(zap.String("host", "10.0.0.1"))
	l.Info("Connection established")

	assert.Contains(t, buf.String(), "db.host=10.0.0.1", "Fields across With must remain in db namespace")
	assert.NotContains(t, buf.String(), " host=10.0.0.1", "Cannot fall back to top level")
}

// Multi-level nesting: With(Namespace("a")) -> With(Namespace("b")) ->
// With(String("k","v")) must render as a.b.k=v -- verifies that Clone's
// namespace replay supports more than one level.
func TestConsoleNestedNamespacePreservedAcrossMultipleWith(t *testing.T) {
	var buf bytes.Buffer
	enc := newConsoleEncoder(false)
	core := zapcore.NewCore(enc, zapcore.AddSync(&buf), zapcore.DebugLevel)
	l := zap.New(core).
		With(zap.Namespace("a")).
		With(zap.Namespace("b")).
		With(zap.String("k", "v"))
	l.Info("m")

	assert.Contains(t, buf.String(), "a.b.k=v")
}

// Clone isolation: the parent encoder and the clone each add fields into
// the same already-open namespace, and neither shows up in the other's
// output. If Fields were only shallow-copied, the clone and parent would
// share the same db sub-map, and a write on one side would leak into the
// other.
func TestConsoleCloneNamespaceIsolation(t *testing.T) {
	base := newConsoleEncoder(false)
	zap.Namespace("db").AddTo(base)
	zap.String("host", "10.0.0.1").AddTo(base)

	clone := base.Clone()
	zap.String("port", "5432").AddTo(clone)

	baseOut, err := base.EncodeEntry(sampleEntry(), nil)
	require.NoError(t, err)
	cloneOut, err := clone.EncodeEntry(sampleEntry(), nil)
	require.NoError(t, err)

	assert.Contains(t, baseOut.String(), "db.host=10.0.0.1")
	assert.NotContains(t, baseOut.String(), "db.port=5432", "Fields added by clone cannot leak back to parent encoder")
	assert.Contains(t, cloneOut.String(), "db.host=10.0.0.1")
	assert.Contains(t, cloneOut.String(), "db.port=5432")
}

// TestConsoleCloneDeepCopiesNestedMap pins down "the copy of Fields must be
// a recursive deep copy", and specifically covers a path that
// TestConsoleCloneNamespaceIsolation does not reach.
//
// Verified (via mutation testing): TestConsoleCloneNamespaceIsolation
// exercises the namespace opened by zap.Namespace, and Clone()'s isolation
// of namespaces is actually achieved as a side effect of the "replay
// OpenNamespace" step -- OpenNamespace always builds a brand-new empty map
// each time and then moves the content in entry by entry, so it naturally
// never shares an object with the old map. That means even if the
// top-level Fields copy were downgraded back to a shallow copy, this test
// would still not fail (verified via mutation: deleting deepCopyFields and
// reverting to a shallow copy still leaves this test green).
//
// The only thing that truly needs the deep copy for protection is a field
// that is not routed through zap.Namespace, but instead hangs a nested
// map[string]any value directly under some key (e.g. zap.Any("m",
// someMap)) -- such a sub-map is not on the path recorded in e.ns, so
// Clone's namespace-replay logic never touches it, and the only thing that
// can cut it off from sharing the same map object with the parent encoder
// is the deep copy itself.
func TestConsoleCloneDeepCopiesNestedMap(t *testing.T) {
	base := newConsoleEncoder(false)
	shared := map[string]any{"k": "v"}
	zap.Any("m", shared).AddTo(base)

	clone := base.Clone()
	// Modify shared only after Clone -- if the clone internally holds the
	// same map object (shallow copy), this modification would show up in
	// the clone's output too; with a deep copy, the clone already has its
	// own independent copy at the moment of Clone(), and is unaffected.
	shared["leaked"] = "yes"

	baseOut, err := base.EncodeEntry(sampleEntry(), nil)
	require.NoError(t, err)
	cloneOut, err := clone.EncodeEntry(sampleEntry(), nil)
	require.NoError(t, err)

	assert.Contains(t, baseOut.String(), "m.leaked=yes", "base holds its own shared, modifying it is normal to see")
	assert.NotContains(t, cloneOut.String(), "m.leaked=yes",
		"clone must obtain an independent copy at the moment of Clone(), modifying shared after Clone() must not leak into clone")
}

func TestConsoleCloneIsolatesFields(t *testing.T) {
	base := newConsoleEncoder(false)
	zap.String("shared", "yes").AddTo(base)

	clone := base.Clone()
	zap.String("only_in_clone", "1").AddTo(clone)

	baseOut, err := base.EncodeEntry(sampleEntry(), nil)
	require.NoError(t, err)
	cloneOut, err := clone.EncodeEntry(sampleEntry(), nil)
	require.NoError(t, err)

	assert.Contains(t, baseOut.String(), "shared=yes")
	assert.NotContains(t, baseOut.String(), "only_in_clone", "Writing to child instance after Clone must not pollute parent instance")
	assert.Contains(t, cloneOut.String(), "shared=yes")
	assert.Contains(t, cloneOut.String(), "only_in_clone=1")
}

func TestConsoleStacktraceAppended(t *testing.T) {
	ent := sampleEntry()
	ent.Level = zapcore.ErrorLevel
	ent.Stack = "goroutine 1 [running]:\nmain.main()"
	out := encodeOne(t, false, ent, nil, nil)
	assert.Contains(t, out, "goroutine 1 [running]:")
	assert.True(t, strings.HasSuffix(out, "\n"))
}

func TestWantColor(t *testing.T) {
	var buf bytes.Buffer // not a *os.File, cannot be a TTY

	assert.True(t, wantColor(ColorAlways, &buf), "always must be enabled unconditionally")
	assert.False(t, wantColor(ColorNever, &buf), "never must be disabled unconditionally")
	assert.False(t, wantColor(ColorAuto, &buf), "auto is off when not TTY")

	t.Setenv("NO_COLOR", "1")
	assert.False(t, wantColor(ColorAuto, &buf))
	assert.True(t, wantColor(ColorAlways, &buf), "explicit always overrides NO_COLOR")
}

func TestWantColorRespectsDumbTerm(t *testing.T) {
	var buf bytes.Buffer
	t.Setenv("TERM", "dumb")
	assert.False(t, wantColor(ColorAuto, &buf))
}

// TestConsoleDemo writes one line per level to stdout for eyeball
// verification of alignment and coloring.
// Kept permanently in the repo -- run it casually after touching the
// encoder:
//
//	go test ./log/ -run TestConsoleDemo -v
//
// Eyeball verification cannot be asserted, but "every line was actually
// produced" can be -- a case that only looks and does not assert would
// still pass green if the encoder silently returned an empty string,
// providing no guard at all.
func TestConsoleDemo(t *testing.T) {
	if testing.Short() {
		t.Skip("demo case, skipped under -short")
	}
	// Write to both stdout (for humans) and buf (for assertions)
	var buf bytes.Buffer
	sink := zapcore.NewMultiWriteSyncer(zapcore.Lock(os.Stdout), zapcore.AddSync(&buf))
	enc := newConsoleEncoder(true) // force coloring, so the effect is visible even on a non-TTY
	core := zapcore.NewCore(enc, sink, zapcore.DebugLevel)
	l := zap.New(core, zap.AddCaller()).
		With(zap.String("trace_id", "01926f7e1a2b3c4d5e6f708192a3b4c5"))

	l.Debug("Connection pool is ready", zap.Int("size", 10))
	l.Info("Validation passed", zap.Int("order_id", 1001), zap.Float64("amount", 99.5))
	l.Warn("Retry", zap.Int("attempt", 2), zap.Duration("backoff", 300*time.Millisecond))
	l.Error("Payment failed", zap.Error(errors.New("connection refused")))
	l.Info("Value contains space", zap.String("reason", "café balance"))

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 5, "Five calls must produce exactly five lines")
	for i, ln := range lines {
		assert.NotEmpty(t, strings.TrimSpace(stripANSI(ln)), "Line %d cannot be empty", i+1)
	}
	assert.Contains(t, buf.String(), "café balance", "multibyte text must be preserved unchanged")
	assert.Contains(t, buf.String(), `reason="café balance"`, "Values with space must be quoted")
	assert.NotContains(t, buf.String(), "trace_id=", "trace_id occupies a fixed column, should not enter KV area")
}
