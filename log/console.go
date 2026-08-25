package log

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mattn/go-isatty"
	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

// ANSI color codes. Zero width, so they do not affect alignment.
const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[90m" // bright black = dim gray
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

// Fixed widths of each column.
//
// widthLevel is 6, not 5: the longest zapcore level text is DPANIC (6
// characters, measured: `zapcore.DPanicLevel.CapitalString()` == "DPANIC").
// Using 5 would leave the string unpadded whenever its rune count >= w,
// shifting the four trailing columns of the DPanic line one space to the
// right.
//
// widthCaller is 24, per the spec §8.7 table.
//
// callerText uses zapcore.EntryCaller.TrimmedPath(), which always keeps only
// the last two path segments (parent-dir/file.go:line), regardless of how
// deep the original path is -- measured: a deep path like
// "/a/very/deeply/.../long/handler.go" still produces TrimmedPath output of
// just "long/handler.go:1234", 20 characters. In other words the column
// width must be sized off the length distribution of "parent dir name +
// file name + line number" themselves, not off path depth: no matter how
// deep the path, TrimmedPath only ever keeps two segments.
//
// 24 is wide enough to hold the spec §8.7 example line
// "payment/client.go:33" (20 characters) without truncation, while not being
// unreasonably large -- a long package name like
// "verylongpackagename/handlerimplementation.go:33" still gets truncated, so
// the truncation rule itself is not weakened.
//
// A pitfall for whoever touches this next: when verifying the "truncate on
// overflow" rule, test cases cannot pad the length by adding more path
// depth -- TrimmedPath only looks at the last two segments, and a deeper
// path can actually produce a shorter string (because the parent dir name
// might be short, e.g. "long"). To make TrimmedPath longer, the file name or
// parent dir name itself must be made longer, e.g.
// "verylongpackagename/handlerimplementation.go:1234" -> 49 characters
// (this is TrimmedPath's output, which drops everything above the parent
// dir -- do not prepend "/src/" here, that would make it 54).
const (
	widthLevel  = 6
	widthTrace  = 8
	widthCaller = 24

	// consoleTimeLayout has no date -- the date lives in the log file name.
	// widthTime is derived from it rather than written as 12, so the two can
	// never drift apart.
	consoleTimeLayout = "15:04:05.000"
	widthTime         = len(consoleTimeLayout)
)

// Fields excluded from the KV section under console: trace_id already has
// its own fixed column, and the other two are 128/64 bit IDs that add no
// value crammed into a human-readable line.
// The json sink does not apply this exclusion -- that one is for machine
// search.
var consoleHiddenFields = map[string]struct{}{
	"trace_id":   {},
	"span_id":    {},
	"request_id": {},
}

var consolePool = buffer.NewPool()

// consoleEncoder renders a single human-readable line.
//
// It embeds *zapcore.MapObjectEncoder to get all of ObjectEncoder's Add*
// methods for free, and only needs to add Clone and EncodeEntry itself to
// satisfy zapcore.Encoder.
//
// That embedding is also where most of this encoder's allocations come from:
// profiling one entry (12 allocs/op) put NewMapObjectEncoder plus its Add*
// methods at roughly half the total, because every field is routed through a
// map[string]any before it can be grouped (With context vs call site) and
// sorted by key. Streaming fields straight into the buffer would remove that,
// at the cost of rebuilding both the grouping and the sorting by hand.
//
// Deliberately not done. This encoder targets a developer's terminal;
// production writes json, which measures 2 allocs/op on the same entry and is
// untouched by any of this. The column writers below were worth fixing because
// they are self-contained; the map routing is load-bearing structure.
type consoleEncoder struct {
	*zapcore.MapObjectEncoder
	color bool
	// ns is the currently open namespace path, e.g. after
	// With(Namespace("a")).With(Namespace("b")) it is []string{"a", "b"}.
	//
	// MapObjectEncoder tracks "which layer the next field should be written
	// into" with the unexported cur field. A MapObjectEncoder produced by
	// Clone has no access to that private field, so its cloned cur always
	// ends up at the root layer. zapcore.ioCore.With is implemented as
	// "Clone an encoder, then AddTo the fields for this call into it";
	// when zap.Namespace("db") and the following fields are split across
	// two separate With calls, if the encoder cloned for the second call
	// does not know it should be sitting inside db, the field falls from
	// db.host down to the top-level host. So we keep our own copy of ns
	// here, and Clone replays OpenNamespace layer by layer to bring cur
	// back to the same level.
	ns []string
}

func newConsoleEncoder(color bool) zapcore.Encoder {
	return &consoleEncoder{MapObjectEncoder: zapcore.NewMapObjectEncoder(), color: color}
}

// OpenNamespace opens a namespace and records the path into e.ns at the
// same time, for Clone to replay.
func (e *consoleEncoder) OpenNamespace(k string) {
	e.MapObjectEncoder.OpenNamespace(k)
	e.ns = append(e.ns, k)
}

func (e *consoleEncoder) Clone() zapcore.Encoder {
	c := &consoleEncoder{MapObjectEncoder: zapcore.NewMapObjectEncoder(), color: e.color}

	// Deep-copy the fields into c.Fields -- the existing c.Fields map must be
	// the target (not a replacement), because NewMapObjectEncoder()'s private
	// cur already points at it; swapping c.Fields for a different object would
	// leave cur dangling at the old, empty map.
	//
	// Leaf values (string/numeric/[]byte etc.) have value semantics and are
	// safe to assign directly. Nested map[string]any sub-layers (produced by
	// OpenNamespace or zap.Any with a map value) must be recursively copied;
	// a shallow copy would make clone and parent share the same map object,
	// leaking writes between them (TestConsoleCloneDeepCopiesNestedMap pins
	// this down).
	for k, v := range e.Fields {
		if sub, ok := v.(map[string]any); ok {
			c.Fields[k] = deepCopyFields(sub)
			continue
		}
		c.Fields[k] = v
	}

	// Replay the namespace path layer by layer, so c's cur ends up at the
	// same depth as e's.
	//
	// It is not safe to "copy the whole tree first, then call OpenNamespace
	// for each layer": zapcore.MapObjectEncoder.OpenNamespace is
	// implemented to unconditionally overwrite cur[k] with a brand-new
	// empty map and then point cur at it (see
	// go.uber.org/zap/zapcore/memory_encoder.go). If the content had
	// already been copied in before this step, OpenNamespace would wipe
	// out everything just copied in. So it must be done layer by layer:
	//   1. Save this layer's deep-copied old content (saved);
	//   2. Call c.OpenNamespace(k), which swaps curMap[k] for a new empty
	//      map and points cur at it -- because curMap and the cur inside
	//      OpenNamespace were the same map object before being swapped, we
	//      can read this new map straight out of curMap[k] afterward,
	//      without touching the private field;
	//   3. Move saved's content into this new map;
	//   4. Advance curMap to this new map and move on to the next layer.
	curMap := c.Fields
	for _, k := range e.ns {
		saved, _ := curMap[k].(map[string]any)
		c.OpenNamespace(k)
		next, _ := curMap[k].(map[string]any)
		for kk, vv := range saved {
			next[kk] = vv
		}
		curMap = next
	}
	return c
}

// deepCopyFields recursively deep-copies m: the top level and every nested
// map[string]any sub-layer get their own new map; leaf values are moved as
// is (string/numeric/[]byte etc. already have value semantics or are
// immutable, so they need no further copying). Used by Clone -- see the
// comment above.
func deepCopyFields(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			out[k] = deepCopyFields(sub)
			continue
		}
		out[k] = v
	}
	return out
}

func (e *consoleEncoder) EncodeEntry(ent zapcore.Entry, fs []zapcore.Field) (*buffer.Buffer, error) {
	// Collect this call's fields separately, so they can be grouped in the
	// output apart from the With context fields.
	call := zapcore.NewMapObjectEncoder()
	for _, f := range fs {
		f.AddTo(call)
	}

	b := consolePool.Get()

	// (1) time: fixed widthTime wide, no date -- the date is in the file name
	e.paintTime(b, ent.Time)
	b.AppendByte(' ')

	// (2) level: left-aligned to 6 wide (widthLevel), colored by level
	e.paintPadRight(b, levelColor(ent.Level), ent.Level.CapitalString(), widthLevel)
	b.AppendByte(' ')

	// (3) trace: fixed 8 wide, first 8 chars of trace_id
	e.paintPadRight(b, ansiDim, shortTrace(e.Fields, call.Fields), widthTrace)
	b.AppendByte(' ')

	// (4) caller: right-aligned to widthCaller wide, truncated from the left
	// when too long
	e.paintPadCallerLeft(b, ansiDim, callerText(ent.Caller), widthCaller)
	b.AppendByte(' ')

	// (5) msg, followed by two spaces and then the KV section
	b.AppendString(ent.Message)
	b.AppendString("  ")

	// (6) KV: With context fields first, then this call's fields, each
	// sorted alphabetically by key
	first := true
	e.writeFields(b, e.Fields, &first)
	e.writeFields(b, call.Fields, &first)

	if ent.Stack != "" {
		b.AppendByte('\n')
		b.AppendString(ent.Stack)
	}
	b.AppendByte('\n')
	return b, nil
}

func (e *consoleEncoder) writeFields(b *buffer.Buffer, m map[string]any, first *bool) {
	e.writeFieldsPrefixed(b, m, "", first, 0)
}

// maxConsoleDepth is the upper bound on how many levels of nesting get
// flattened. A self-referential map would otherwise make the recursion
// never terminate, and a human-readable line has no need for structures
// nested more than eight levels deep anyway.
//
// Measured: once the limit is hit, we cannot fall back to the originally
// planned fmt.Sprint(v) to print the whole sub-map -- the fmt package has
// no cycle detection at all for map values (only pointer-like Kinds record
// visited). Calling fmt.Sprint on a self-referential map recurses forever
// inside fmt.(*pp).printValue -- not "slow", but an outright stack
// overflow that crashes the process (fatal error: stack overflow, not even
// recover can save it). So once the limit is hit we can only print a
// placeholder, and must never call stringify/fmt.Sprint on a map value.
const maxConsoleDepth = 8

// writeFieldsPrefixed recursively flattens nested maps, joining levels into
// a full dotted-path key.
//
// Why flattening is needed: fields after zap.Namespace("db") are not
// spread flat at the top level -- MapObjectEncoder.OpenNamespace collects
// them into a nested map, leaving only the single "db" key at the top.
// Measured: after Namespace("ns"), all three fields AddTo'd afterward land
// in ns's sub-map, and the top level has len == 1. Stringifying it directly
// would print Go's map[k:v] literal, which is both hard to read, and would
// make consoleHiddenFields' exclusion completely ineffective at the nested
// layer too.
//
// Flattening it into db.host=... db.port=... solves both problems at once:
// the shape matches the json sink's nested semantics one to one (json has
// {"db":{"host":...}}), and the exclusion check can now see the full path.
// A map[string]any passed directly by the caller is flattened the same
// way -- so its shape is consistent with namespace-produced maps.
func (e *consoleEncoder) writeFieldsPrefixed(b *buffer.Buffer, m map[string]any, prefix string, first *bool, depth int) {
	if len(m) == 0 {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		full := k
		if prefix != "" {
			full = prefix + "." + k
		}
		if _, hidden := consoleHiddenFields[full]; hidden {
			continue
		}

		// Nested maps keep getting flattened; an empty sub-map is skipped
		// entirely, so we never leave a lonely key= behind.
		if sub, ok := m[k].(map[string]any); ok {
			if depth < maxConsoleDepth {
				e.writeFieldsPrefixed(b, sub, full, first, depth+1)
				continue
			}
			// Depth limit hit: we must never call stringify on a map value
			// (fmt.Sprint has no cycle detection for maps, and a
			// self-referential map will crash with a stack overflow --
			// measured), so we print only a placeholder.
			e.writeLeaf(b, full, "<depth-limit>", "", first)
			continue
		}

		s := stringify(m[k])
		e.writeLeaf(b, full, s, k, first)
	}
}

// writeLeaf writes a single key=value. When errKey is non-empty, the value
// is highlighted as an err/error key.
//
// A pitfall found by testing: the original approach was paint(cyan, key)
// followed by writing '=' and value separately -- this inserts ansiReset
// between key and '=', so "key=value" is no longer contiguous in the raw
// colored bytes. TestConsoleDemo's literal Contains(`reason="余额 不足"`)
// assertion against buf.String() (which keeps the color codes) gets broken
// by this reset sitting in the middle, and the assertion fails.
// Fix: for non-error keys, assemble the whole "key=value" string first and
// then wrap the entire thing in a single ansiCyan/ansiReset pair, so the
// color codes only ever appear at the outer edges of the whole segment and
// never interrupt readable text in the middle.
// error keys still get key colored cyan and value colored red as two
// separate segments -- no test case requires these two segments to be
// byte-contiguous, and coloring them separately still satisfies both "key
// is cyan" and "err's value is red" at the same time.
func (e *consoleEncoder) writeLeaf(b *buffer.Buffer, full, s, errKey string, first *bool) {
	if !*first {
		b.AppendByte(' ')
	}
	*first = false

	if e.color && isErrorKey(errKey) {
		e.paint(b, ansiCyan, full)
		b.AppendByte('=')
		b.AppendString(ansiRed)
		writeConsoleValue(b, s)
		b.AppendString(ansiReset)
		return
	}

	if !e.color {
		b.AppendString(full)
		b.AppendByte('=')
		writeConsoleValue(b, s)
		return
	}

	tmp := consolePool.Get()
	tmp.AppendString(full)
	tmp.AppendByte('=')
	writeConsoleValue(tmp, s)
	e.paint(b, ansiCyan, tmp.String())
	tmp.Free()
}

// paint writes colored text. When color is off, only the text is written.
// Pad first, then colorize -- ANSI sequences are zero width and do not
// break alignment.
func (e *consoleEncoder) paint(b *buffer.Buffer, c, s string) {
	if e.color {
		b.AppendString(c)
		b.AppendString(s)
		b.AppendString(ansiReset)
		return
	}
	b.AppendString(s)
}

func levelColor(lv zapcore.Level) string {
	switch lv {
	case zapcore.DebugLevel:
		return ansiCyan
	case zapcore.WarnLevel:
		return ansiYellow
	case zapcore.ErrorLevel, zapcore.DPanicLevel, zapcore.PanicLevel, zapcore.FatalLevel:
		return ansiRed
	default:
		return ansiGreen
	}
}

func isErrorKey(k string) bool { return k == "err" || k == "error" }

// shortTrace takes the first 8 characters of trace_id. Returns an empty
// string when there is no trace (paintPadRight then pads it out to a blank
// column).
func shortTrace(ms ...map[string]any) string {
	for _, m := range ms {
		if v, ok := m["trace_id"].(string); ok && v != "" {
			if len(v) > widthTrace {
				return v[:widthTrace]
			}
			return v
		}
	}
	return ""
}

func callerText(c zapcore.EntryCaller) string {
	if !c.Defined {
		return ""
	}
	return c.TrimmedPath()
}

// paintPadRight / paintPadCallerLeft count by rune, not by byte.
//
// Of the three leading columns (level, trace, caller), level and trace are
// always ASCII, but caller comes from a Go source file path, and a user's
// directory names can be Chinese. Measured consequence of counting by byte:
// padding "中文" to 5 leaves it untouched because len == 6 >= 5, even though
// it actually only occupies 2 display columns, misaligning the whole line.
//
// This only does rune alignment, not East Asian double-width handling (a
// CJK rune occupies two terminal columns) -- that would require an
// out-of-scope dependency like runewidth, and these three columns are
// ASCII-dominated anyway. Conclusion: a caller path containing CJK will
// still have an off display width, but will no longer produce invalid
// UTF-8.
//
// They write into the buffer rather than returning a padded string. Building
// the string first cost one allocation per column per entry -- for a value
// the buffer was about to copy and discard. console_column_test.go keeps the
// string-building versions as the reference these are checked against.

// consoleSpaces is the source of every run of padding. Slicing a constant
// string costs nothing, where strings.Repeat allocates. It is sized to the
// widest column so a single AppendString covers the common case.
const consoleSpaces = "                        " // widthCaller spaces

func appendSpaces(b *buffer.Buffer, n int) {
	for n > len(consoleSpaces) {
		b.AppendString(consoleSpaces)
		n -= len(consoleSpaces)
	}
	if n > 0 {
		b.AppendString(consoleSpaces[:n])
	}
}

// consoleTimeBufSize is the scratch array paintTime formats into. It is
// deliberately NOT widthTime.
//
// Measured: AppendFormat overshoots the final length while building the
// output, so a widthTime-sized (12) array forces a heap reallocation and the
// whole point of avoiding Time.Format is lost -- 16 still allocates, 20 does
// not. 32 leaves headroom above that boundary. Tightening this to "the width
// it actually produces" is the obvious-looking edit that silently reintroduces
// the allocation; TestConsoleColumnWritersAreAllocationFree is what catches it.
const consoleTimeBufSize = 32

// paintTime writes the timestamp column. It formats into a stack array via
// AppendFormat: Time.Format allocates a fresh string on every entry, and this
// runs once per line.
func (e *consoleEncoder) paintTime(b *buffer.Buffer, t time.Time) {
	var tbuf [consoleTimeBufSize]byte
	if e.color {
		b.AppendString(ansiDim)
	}
	_, _ = b.Write(t.AppendFormat(tbuf[:0], consoleTimeLayout))
	if e.color {
		b.AppendString(ansiReset)
	}
}

// paintPadRight left-aligns s to a width of w. Pad first, then close the
// color -- ANSI sequences are zero width, so the reset belongs after the
// padding, not before it.
func (e *consoleEncoder) paintPadRight(b *buffer.Buffer, c, s string, w int) {
	if e.color {
		b.AppendString(c)
	}
	b.AppendString(s)
	if n := utf8.RuneCountInString(s); n < w {
		appendSpaces(b, w-n)
	}
	if e.color {
		b.AppendString(ansiReset)
	}
}

// paintPadCallerLeft right-aligns to a width of w. When too long, truncates
// from the left and adds "…", keeping the line-number side intact --
// locating code relies on the file name and line number, not the top-level
// directory.
//
// Truncation cuts on rune boundaries. Cutting by byte
// (s[len(s)-(w-1):]) can slice through the middle of a multi-byte
// character, producing a U+FFFD replacement character on output, and the
// resulting width would not even be w.
func (e *consoleEncoder) paintPadCallerLeft(b *buffer.Buffer, c, s string, w int) {
	if e.color {
		b.AppendString(c)
	}
	if n := utf8.RuneCountInString(s); n > w {
		// Drop the leading n-(w-1) runes; the ellipsis takes the freed column.
		// Scanning for the byte offset avoids the []rune conversion.
		cut := 0
		for i := 0; i < n-(w-1); i++ {
			_, size := utf8.DecodeRuneInString(s[cut:])
			cut += size
		}
		b.AppendString("…")
		b.AppendString(s[cut:])
	} else {
		appendSpaces(b, w-n)
		b.AppendString(s)
	}
	if e.color {
		b.AppendString(ansiReset)
	}
}

// stringify converts a field value to a string.
// strconv.Quote is not used -- it would turn Chinese characters into
// \uXXXX, garbling Chinese log content.
func stringify(v any) string {
	switch x := v.(type) {
	case string:
		return x
	// zap.Binary / zap.Any([]byte) end up as []byte in MapObjectEncoder
	// (AddBinary stores v directly). Measured: zap.ByteString does not
	// actually reach this branch -- AddByteString already does string(v)
	// internally, so what's stored in MapObjectEncoder.Fields is already a
	// string, and gets caught by the case string branch above. This
	// []byte branch exists as a fallback for zap.Binary / zap.Any([]byte):
	// without this special case it would print [104 105] instead of hi.
	case []byte:
		return string(x)
	case error:
		return x.Error()
	case fmt.Stringer:
		return x.String()
	case nil:
		return "<nil>"
	default:
		return fmt.Sprint(v)
	}
}

// writeConsoleValue writes a value. When it contains a space, quote, equals
// sign, or newline, it is quoted and escaped, so that a single log entry
// always occupies exactly one line and every field can be grepped in full.
func writeConsoleValue(b *buffer.Buffer, s string) {
	if s == "" {
		b.AppendString(`""`)
		return
	}
	if !strings.ContainsAny(s, " \t\r\n\"=") {
		b.AppendString(s)
		return
	}
	b.AppendByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.AppendString(`\"`)
		case '\\':
			b.AppendString(`\\`)
		case '\n':
			b.AppendString(`\n`)
		case '\r':
			b.AppendString(`\r`)
		case '\t':
			b.AppendString(`\t`)
		default:
			b.AppendString(string(r))
		}
	}
	b.AppendByte('"')
}

// wantColor decides whether to colorize.
// The auto decision order: NO_COLOR unset -> TERM is not dumb -> output is
// a TTY.
func wantColor(mode string, w io.Writer) bool {
	switch mode {
	case ColorAlways:
		return true
	case ColorNever:
		return false
	}
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}
