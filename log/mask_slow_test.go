package log

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertAmortizedZeroAlloc asserts that f allocates nothing per call once the
// pool is warm, measured as the best (minimum) of several AllocsPerRun rounds.
//
// Why best-of-N rather than a single measurement: AllocsPerRun diffs the
// PROCESS-wide runtime.MemStats.Mallocs counter, so every allocation made by
// any other goroutine inside the measurement window is charged to f. Under
// -race, or simply under parallel load, the window stretches from microseconds
// to milliseconds and picks up a lot more of that foreign traffic; the same
// allocation-free code then reports a non-zero average intermittently.
//
// Taking the minimum costs zero guard strength, because the noise is strictly
// one-directional: foreign goroutines can only ADD mallocs to the window, they
// can never cancel out an allocation f really made. So the minimum is the round
// with the least contamination -- never a round where a real allocation went
// missing. If the pooling in hitSlow/hitWindowSlow ever regresses to a fresh
// make per call (2 allocs/call before the fix), every round measures avg == 2
// and the assertion still fails, in every round, on the first one.
func assertAmortizedZeroAlloc(t *testing.T, f func(), msgAndArgs ...any) {
	t.Helper()
	best := math.Inf(1)
	for range 3 {
		if avg := testing.AllocsPerRun(200, f); avg < best {
			best = avg
		}
		if best == 0 {
			return
		}
	}
	assert.Zero(t, best, msgAndArgs...)
}

// The overflow path (a field name longer than maskKeyBufSize, or with more
// words than maskKeyMaxWords) used to allocate a fresh buf+starts pair on
// every call, with starts sized by BYTE count while it only ever holds one
// int per WORD -- an 8x overshoot. Measured before the fix: a 98-byte key
// cost 1008 B / 2 allocs, ~10x amplification of attacker-supplied input.
//
// That matters because map keys reach hit() directly (maskStringKeyMap calls
// m.hit on every key), and map keys routinely come from outside -- a parsed
// JSON body or a query string logged as a single field.

func TestHitSlowIsAmortizedZeroAlloc(t *testing.T) {
	m := newMasker(nil)
	// 98 bytes, over maskKeyBufSize (64), ending in a word that DOES hit.
	longHit := strings.Repeat("verylongsegment_", 6) + "password"
	require.Greater(t, len(longHit), maskKeyBufSize, "Test case must really follow the overflow path")

	assertAmortizedZeroAlloc(t, func() { _ = m.hit(longHit) },
		"Overflow path should amortize zero allocations")
}

func TestHitWindowSlowIsAmortizedZeroAlloc(t *testing.T) {
	m := newMasker(nil)
	manyWords := strings.Repeat("a_", 20) + "token_x" // > maskKeyMaxWords
	assertAmortizedZeroAlloc(t, func() { _ = m.hitWindow(manyWords) },
		"Window match overflow path should amortize zero allocations")
}

// Pooling must not change any verdict: the slow path's rule is identical to
// the fast path's, and reusing a dirty buffer must not leak state between
// calls.
func TestSlowPathVerdictsUnchangedByPooling(t *testing.T) {
	m := newMasker(nil)
	pad := strings.Repeat("verylongsegment_", 6) // 96 bytes of filler

	cases := []struct {
		name string
		key  string
		want bool
	}{
		{"Very long and center word matches", pad + "password", true},
		{"Very long and center word does not match", pad + "count", false},
		{"Very long camel case match", pad + "accessToken", true},
		{"Very long but only contains sensitive word, not center word", pad + "token_count", false},
		{"Exceeds word count match", strings.Repeat("a_", 20) + "secret", true},
		{"Exceeds word count does not match", strings.Repeat("a_", 20) + "name", false},
	}

	// Run the whole set twice: the second pass reuses buffers the first pass
	// dirtied, which is exactly where a length-tracking bug would surface.
	for pass := 1; pass <= 2; pass++ {
		for _, c := range cases {
			assert.Equal(t, c.want, m.hit(c.key), "The %d-th round %s", pass, c.name)
		}
	}
}

// A single pathological key must not leave a pooled buffer permanently huge --
// otherwise one oversized request permanently inflates the process's
// steady-state memory.
func TestSlowPathPoolDoesNotRetainOversizedBuffers(t *testing.T) {
	assert.False(t, maskSlowBufReusable(&maskSlowBuf{buf: make([]byte, maskSlowBufMax+1)}),
		"Buffer exceeding the limit should not be put back into the pool")
	assert.False(t, maskSlowBufReusable(&maskSlowBuf{starts: make([]int, maskSlowBufMax+1)}),
		"Starts exceeding the limit should not be put back into the pool")
	assert.True(t, maskSlowBufReusable(&maskSlowBuf{
		buf:    make([]byte, maskSlowBufMax),
		starts: make([]int, maskSlowBufMax),
	}), "Buffer exactly at the limit should still be reused")

	// The behavioural half: an oversized key is still matched correctly, and
	// the ordinary overflow path stays cheap afterwards.
	m := newMasker(nil)
	huge := strings.Repeat("x", maskSlowBufMax*4) + "_password"
	assert.True(t, m.hit(huge), "Center word should still match for very large key")

	ordinary := strings.Repeat("verylongsegment_", 6) + "password"
	assertAmortizedZeroAlloc(t, func() { _ = m.hit(ordinary) },
		"After very large key, normal overflow key should still amortize zero allocations")
}
