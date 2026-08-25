package log

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	require.Greater(t, len(longHit), maskKeyBufSize, "用例必须真的走溢出路径")

	avg := testing.AllocsPerRun(200, func() { _ = m.hit(longHit) })
	assert.Zero(t, avg, "溢出路径应摊销零分配，实测每次 %v 次分配", avg)
}

func TestHitWindowSlowIsAmortizedZeroAlloc(t *testing.T) {
	m := newMasker(nil)
	manyWords := strings.Repeat("a_", 20) + "token_x" // > maskKeyMaxWords
	avg := testing.AllocsPerRun(200, func() { _ = m.hitWindow(manyWords) })
	assert.Zero(t, avg, "窗口匹配的溢出路径应摊销零分配，实测每次 %v 次分配", avg)
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
		{"超长且中心词命中", pad + "password", true},
		{"超长且中心词不命中", pad + "count", false},
		{"超长驼峰命中", pad + "accessToken", true},
		{"超长但只是含敏感词非中心词", pad + "token_count", false},
		{"超词数命中", strings.Repeat("a_", 20) + "secret", true},
		{"超词数不命中", strings.Repeat("a_", 20) + "name", false},
	}

	// Run the whole set twice: the second pass reuses buffers the first pass
	// dirtied, which is exactly where a length-tracking bug would surface.
	for pass := 1; pass <= 2; pass++ {
		for _, c := range cases {
			assert.Equal(t, c.want, m.hit(c.key), "第 %d 轮 %s", pass, c.name)
		}
	}
}

// A single pathological key must not leave a pooled buffer permanently huge --
// otherwise one oversized request permanently inflates the process's
// steady-state memory.
func TestSlowPathPoolDoesNotRetainOversizedBuffers(t *testing.T) {
	assert.False(t, maskSlowBufReusable(&maskSlowBuf{buf: make([]byte, maskSlowBufMax+1)}),
		"超过上限的 buf 不应放回池中")
	assert.False(t, maskSlowBufReusable(&maskSlowBuf{starts: make([]int, maskSlowBufMax+1)}),
		"超过上限的 starts 不应放回池中")
	assert.True(t, maskSlowBufReusable(&maskSlowBuf{
		buf:    make([]byte, maskSlowBufMax),
		starts: make([]int, maskSlowBufMax),
	}), "恰好在上限的缓冲仍应复用")

	// The behavioural half: an oversized key is still matched correctly, and
	// the ordinary overflow path stays cheap afterwards.
	m := newMasker(nil)
	huge := strings.Repeat("x", maskSlowBufMax*4) + "_password"
	assert.True(t, m.hit(huge), "超大 key 的中心词仍应命中")

	ordinary := strings.Repeat("verylongsegment_", 6) + "password"
	avg := testing.AllocsPerRun(200, func() { _ = m.hit(ordinary) })
	assert.Zero(t, avg, "超大 key 之后普通溢出 key 仍应摊销零分配")
}
