package log

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

// The three fixed-width columns and the timestamp are written on every single
// console entry. Building them as strings first (strings.Repeat, []rune,
// Time.Format) meant four allocations per line that the buffer was going to
// copy and throw away anyway. These tests pin down both halves of the
// replacement: the writers allocate nothing, and they still produce byte-for-
// byte what the string-building versions produced.

// padRightRef and padCallerLeftRef are the string-building implementations the
// buffer writers replaced. They stay here deliberately: a padding regression
// shows up as two independent implementations disagreeing, which is a stronger
// signal than a hand-written expected value that was copied from the code it
// is supposed to check.
func padRightRef(s string, w int) string {
	n := utf8.RuneCountInString(s)
	if n >= w {
		return s
	}
	return s + strings.Repeat(" ", w-n)
}

func padCallerLeftRef(s string, w int) string {
	rs := []rune(s)
	if len(rs) > w {
		return "…" + string(rs[len(rs)-(w-1):])
	}
	return strings.Repeat(" ", w-len(rs)) + s
}

func TestConsoleColumnWritersAreAllocationFree(t *testing.T) {
	ts := time.Date(2026, 8, 24, 10, 23, 45, 123e6, time.UTC)

	for _, color := range []bool{false, true} {
		e := newConsoleEncoder(color).(*consoleEncoder)
		b := consolePool.Get()

		avg := testing.AllocsPerRun(200, func() {
			b.Reset()
			e.paintTime(b, ts)
			e.paintPadRight(b, ansiGreen, "INFO", widthLevel)
			e.paintPadRight(b, ansiDim, "01926f7e", widthTrace)
			// Both branches: the short one pads, the long one truncates.
			e.paintPadCallerLeft(b, ansiDim, "order/service.go:42", widthCaller)
			e.paintPadCallerLeft(b, ansiDim,
				"verylongpackagename/handlerimplementation.go:1234", widthCaller)
		})
		b.Free()

		assert.Zero(t, avg, "color=%v 时定宽列与时间格式化都不该分配，实测每次 %v 次", color, avg)
	}
}

// The writers replaced pure functions, so equality against those functions is
// the whole correctness argument. CJK cases are in the table for the reason
// spelled out above padRight's old comment: counting by byte instead of by
// rune silently misaligns the line.
func TestConsolePadWritersMatchReference(t *testing.T) {
	cases := []string{
		"",
		"INFO",
		"DPANIC",                // exactly widthLevel, the "no padding" boundary
		"01926f7e",              // exactly widthTrace
		"中文",                    // shorter than its byte length
		"order/service.go:42",   // short caller, gets padded
		"payment/client.go:33",  // spec §8.7 example, must not truncate
		"a/b.go:7",              //
		strings.Repeat("中", 40), // long CJK, must cut on a rune boundary
		"verylongpackagename/handlerimplementation.go:1234", // long ASCII, truncates
	}

	e := newConsoleEncoder(false).(*consoleEncoder)
	for _, s := range cases {
		for _, w := range []int{widthLevel, widthTrace, widthCaller} {
			b := consolePool.Get()
			e.paintPadRight(b, "", s, w)
			assert.Equal(t, padRightRef(s, w), b.String(), "paintPadRight(%q, %d)", s, w)
			b.Free()

			b = consolePool.Get()
			e.paintPadCallerLeft(b, "", s, w)
			got := b.String()
			assert.Equal(t, padCallerLeftRef(s, w), got, "paintPadCallerLeft(%q, %d)", s, w)
			assert.True(t, utf8.ValidString(got), "截断必须落在 rune 边界上: %q", got)
			b.Free()
		}
	}
}

// paintTime must render the same layout Time.Format did, and the column has to
// stay exactly widthTime wide or every column to its right shifts.
func TestConsolePaintTimeMatchesFormat(t *testing.T) {
	for _, ts := range []time.Time{
		time.Date(2026, 8, 24, 10, 23, 45, 123e6, time.UTC),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),          // midnight, all-zero fields
		time.Date(2026, 12, 31, 23, 59, 59, 999e6, time.UTC), // upper end of every field
	} {
		e := newConsoleEncoder(false).(*consoleEncoder)
		b := consolePool.Get()
		e.paintTime(b, ts)
		got := b.String()
		b.Free()

		assert.Equal(t, ts.Format(consoleTimeLayout), got)
		assert.Len(t, got, widthTime, "时间列必须恰好 widthTime 宽，否则右边所有列都会错位")
	}
}

// The color path must wrap the whole column, padding included -- ANSI codes are
// zero width, so the reset has to sit after the spaces, not before them.
func TestConsoleColumnWritersWrapPaddingInColor(t *testing.T) {
	e := newConsoleEncoder(true).(*consoleEncoder)

	b := consolePool.Get()
	e.paintPadRight(b, ansiGreen, "INFO", widthLevel)
	assert.Equal(t, ansiGreen+"INFO  "+ansiReset, b.String(), "补白必须在 reset 之前")
	b.Free()

	b = consolePool.Get()
	e.paintPadCallerLeft(b, ansiDim, "a/b.go:7", widthCaller)
	assert.Equal(t, ansiDim+strings.Repeat(" ", widthCaller-len("a/b.go:7"))+"a/b.go:7"+ansiReset,
		b.String(), "右对齐的补白同样必须包在颜色里")
	b.Free()
}

// appendSpaces has to survive a run longer than its source string, otherwise a
// wider column silently comes out short.
func TestAppendSpacesHandlesRunsLongerThanTheSource(t *testing.T) {
	for _, n := range []int{0, 1, len(consoleSpaces) - 1, len(consoleSpaces), len(consoleSpaces) + 1, len(consoleSpaces)*3 + 5} {
		b := consolePool.Get()
		appendSpaces(b, n)
		assert.Equal(t, strings.Repeat(" ", n), b.String(), "appendSpaces(%d)", n)
		b.Free()
	}
}
