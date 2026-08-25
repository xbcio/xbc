package log

import (
	"sync"

	"go.uber.org/zap/zapcore"
)

// maskPlaceholder is the value that replaces a masked sensitive field.
const maskPlaceholder = "***"

// builtinMaskFields is the built-in masking blacklist. It is always active and
// cannot be removed via configuration. It follows the org security policy's
// "absolute log blacklist" plus common variants.
//
// There are thousands of call sites; expecting every one of them to remember
// to mask is unrealistic. This is the single interception point.
var builtinMaskFields = []string{
	// Passwords
	"password", "passwd", "pwd", "old_password", "new_password",
	// Password confirmation: the value is the plaintext password itself, not
	// metadata about the password (contrast with password_hash, which does NOT
	// hit -- that's a hash, worth keeping for troubleshooting). The suffix-word-group
	// rule derives confirm / repeat as the head word for these entries, so the rule
	// alone can't catch them; they have to be added as whole-string entries.
	"password_confirm", "password2", "password_repeat",
	// Tokens
	"token", "ulp-token", "access_token", "refresh_token", "id_token",
	"authorization", "cookie", "set-cookie", "session_id", "jwt",
	// Secrets/keys
	"secret", "client_secret", "private_key", "api_key",
	"ak", "sk", "access_key", "access_key_id", "secret_key", "secret_access_key",
	// Connection strings
	"db_url", "dsn", "database_url", "conn_str",
	// Personal information
	"id_card", "bank_card", "credit_card", "card_no", "cvv", "phone", "mobile",
}

// masker decides whether a field name needs masking.
type masker struct{ keys map[string]struct{} }

// newMasker builds a masker from the built-in blacklist plus extra. extra can
// only append entries; it cannot remove built-in ones.
func newMasker(extra []string) *masker {
	m := &masker{keys: make(map[string]struct{}, len(builtinMaskFields)+len(extra))}
	for _, k := range builtinMaskFields {
		m.keys[normalizeMaskKey(k)] = struct{}{}
	}
	for _, k := range extra {
		if nk := normalizeMaskKey(k); nk != "" {
			m.keys[nk] = struct{}{}
		}
	}
	return m
}

// ---------------------------------------------------------------------------
// Field name matching: normalization + suffix word groups
// ---------------------------------------------------------------------------

// Normalization splits a field name into words at separators (_ - . space) and
// camelCase boundaries, lowercases everything, and drops the separators. This
// folds accessToken / access_token / access-token / ACCESS_TOKEN into the same
// string.
//
// The matching rule is whether any "suffix word group" is in the blacklist,
// not a whole-string exact match:
//
//	db_password → [db, password]  → look up "password" (hit), "dbpassword"
//	x-api-key   → [x, api, key]   → look up "key", "apikey" (hit), "xapikey"
//	token_count → [token, count]  → look up "count", "tokencount" -> no hit
//
// Linguistic basis: in English compound nouns, the head word sits at the tail.
// db_password's head is password -- the field is the password itself, and db_
// merely scopes it; token_count's head is count -- the field is metadata about
// a token, not the token itself. This rule isn't a heuristic patch; it has a
// real basis.
//
// The longest suffix word group is the whole string, so the old whole-string
// exact match is a special case of the new rule -- private_key -> "privatekey"
// still hits, phone_masked still doesn't.
const (
	// maskKeyBufSize / maskKeyMaxWords are the capacities of hit's stack buffer.
	// Exceeding them falls back to the heap-allocation path; correctness is
	// unchanged, only slower.
	maskKeyBufSize  = 64
	maskKeyMaxWords = 12
)

func isMaskUpper(c byte) bool { return c >= 'A' && c <= 'Z' }
func isMaskLower(c byte) bool { return c >= 'a' && c <= 'z' }
func isMaskDigit(c byte) bool { return c >= '0' && c <= '9' }

// splitMaskKey writes the normalized key into buf and records each word's
// starting index within buf in starts. It returns the normalized length n,
// the word count wc, and whether buf/starts had enough capacity.
//
// Because separators are all dropped, concatenating words i..end is simply
// buf[starts[i]:n] -- suffix word groups need no further concatenation, which
// is exactly what makes the zero-allocation path possible.
//
// CamelCase word splitting must handle consecutive uppercase letters correctly:
// AK -> [ak] (must not be split into a / k); accessToken -> [access, token];
// xAPIKey -> [x, api, key] (when an uppercase run is followed by a lowercase
// letter, split before the last uppercase letter).
func splitMaskKey(key string, buf []byte, starts []int) (n, wc int, ok bool) {
	newWord := true
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch c {
		case '_', '-', '.', ' ':
			newWord = true
			continue
		}
		if isMaskUpper(c) {
			if !newWord {
				// newWord being false means key[i-1] is definitely not a separator
				prev := key[i-1]
				switch {
				case isMaskLower(prev) || isMaskDigit(prev):
					newWord = true
				case isMaskUpper(prev) && i+1 < len(key) && isMaskLower(key[i+1]):
					newWord = true
				}
			}
			c += 'a' - 'A'
		}
		if newWord {
			if wc >= len(starts) {
				return 0, 0, false
			}
			starts[wc] = n
			wc++
			newWord = false
		}
		if n >= len(buf) {
			return 0, 0, false
		}
		buf[n] = c
		n++
	}
	return n, wc, true
}

// normalizeMaskKey returns the whole-string normalized result; it is only
// called when newMasker builds the blacklist. It shares splitMaskKey with hit,
// so the normalization rules on both sides can never drift apart.
func normalizeMaskKey(k string) string {
	if k == "" {
		return ""
	}
	buf := make([]byte, len(k))
	starts := make([]int, len(k))
	n, _, ok := splitMaskKey(k, buf, starts)
	if !ok {
		return ""
	}
	return string(buf[:n])
}

// hit decides whether a field name needs masking.
//
// It is called for every field of every log entry (and again for nested
// objects), so it is a genuine hot path; common field names therefore go
// through a stack buffer with zero allocations: the compiler has a special
// optimization for m.keys[string(buf[a:b])] that avoids allocating for the
// []byte->string conversion.
func (m *masker) hit(key string) bool {
	if key == "" {
		return false
	}
	var buf [maskKeyBufSize]byte
	var starts [maskKeyMaxWords]int
	n, wc, ok := splitMaskKey(key, buf[:], starts[:])
	if !ok {
		return m.hitSlow(key)
	}
	// Look up from the shortest suffix word group to the longest (the longest
	// being the whole string)
	for i := wc - 1; i >= 0; i-- {
		if _, found := m.keys[string(buf[starts[i]:n])]; found {
			return true
		}
	}
	return false
}

// maskSlowBuf is the buf/starts pair the overflow path borrows instead of
// allocating.
//
// Field names reach hit straight from outside the process: maskStringKeyMap
// runs every map key through it, and map keys routinely come from a parsed
// request body or a query string. Allocating per call let the caller's input
// size dictate a per-key, per-entry allocation -- correct, but an allocation
// amplifier anyone could turn up by sending longer names. Pooling makes the
// overflow path amortized allocation-free.
type maskSlowBuf struct {
	buf    []byte
	starts []int
}

// maskSlowBufMax caps what may go back into the pool. Without it a single
// pathological field name would leave a pooled pair permanently that large,
// turning one oversized request into a permanent memory floor.
const maskSlowBufMax = 1024

var maskSlowPool = sync.Pool{New: func() any { return new(maskSlowBuf) }}

// maskSlowBufReusable reports whether a pair is small enough to keep around.
func maskSlowBufReusable(sb *maskSlowBuf) bool {
	return cap(sb.buf) <= maskSlowBufMax && cap(sb.starts) <= maskSlowBufMax
}

// getMaskSlowBuf returns a pair sized for a key of n bytes. n bounds both:
// the normalized form is never longer than the input, and the word count tops
// out at one word per byte ("aBcD" is four words).
func getMaskSlowBuf(n int) *maskSlowBuf {
	sb := maskSlowPool.Get().(*maskSlowBuf)
	if cap(sb.buf) < n {
		sb.buf = make([]byte, n)
	}
	sb.buf = sb.buf[:n]
	if cap(sb.starts) < n {
		sb.starts = make([]int, n)
	}
	sb.starts = sb.starts[:n]
	return sb
}

func putMaskSlowBuf(sb *maskSlowBuf) {
	if maskSlowBufReusable(sb) {
		maskSlowPool.Put(sb)
	}
}

// hitSlow is the fallback path for overly long field names or too many words:
// it switches to a pooled heap buffer; the rule is identical.
func (m *masker) hitSlow(key string) bool {
	sb := getMaskSlowBuf(len(key))
	defer putMaskSlowBuf(sb)
	n, wc, ok := splitMaskKey(key, sb.buf, sb.starts)
	if !ok {
		// Sized for the worst case, so it cannot overflow
		return false
	}
	for i := wc - 1; i >= 0; i-- {
		if _, found := m.keys[string(sb.buf[sb.starts[i]:n])]; found {
			return true
		}
	}
	return false
}

// hitWindow decides whether any **contiguous word window** of a field name
// hits the blacklist, not just a suffix word group.
//
// Only the replacement-name candidate in hitFieldName goes through it. The
// replacement name is only constructed when a json tag has been corrupted by
// reserved characters, and on that path garbage can sit on both sides of the
// sensitive head word at once (`json:"db\password\x"` normalizes to the three
// words db / password / x), so a suffix word group can never reach a password
// sandwiched in the middle. Normal field names always go through hit's suffix
// rule; the looseness of window matching never leaks onto them.
func (m *masker) hitWindow(key string) bool {
	if key == "" {
		return false
	}
	var buf [maskKeyBufSize]byte
	var starts [maskKeyMaxWords]int
	n, wc, ok := splitMaskKey(key, buf[:], starts[:])
	if !ok {
		return m.hitWindowSlow(key)
	}
	return m.matchWindow(buf[:], starts[:wc], n)
}

// hitWindowSlow is the fallback path for overly long names or too many words;
// the rule is identical to hitWindow.
func (m *masker) hitWindowSlow(key string) bool {
	sb := getMaskSlowBuf(len(key))
	defer putMaskSlowBuf(sb)
	n, wc, ok := splitMaskKey(key, sb.buf, sb.starts)
	if !ok {
		// Sized for the worst case, so it cannot overflow
		return false
	}
	return m.matchWindow(sb.buf, sb.starts[:wc], n)
}

// matchWindow enumerates every contiguous word window [i, j). With the word
// count capped at 12, the worst case is 78 lookups, and it only happens for
// corrupted tags -- never on the hot path.
func (m *masker) matchWindow(buf []byte, starts []int, n int) bool {
	for i := range starts {
		for j := i + 1; j <= len(starts); j++ {
			end := n
			if j < len(starts) {
				end = starts[j]
			}
			if _, found := m.keys[string(buf[starts[i]:end])]; found {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Field filtering
// ---------------------------------------------------------------------------

// apply returns the masked field slice. When nothing hits, it returns the
// input as-is with no allocation at all. When something hits, it copies
// before mutating, and never corrupts the caller's slice.
func (m *masker) apply(fs []zapcore.Field) []zapcore.Field {
	var out []zapcore.Field
	for i := range fs {
		nf, changed := m.maskField(fs[i], 0)
		if !changed {
			continue
		}
		if out == nil {
			out = make([]zapcore.Field, len(fs))
			copy(out, fs)
		}
		out[i] = nf
	}
	if out == nil {
		return fs
	}
	return out
}

// maskField masks a single field. When changed is false, the caller must use
// the original field.
//
// The order is to check f.Key first -- if the key hits, replace the whole
// field with *** and there's no need to look inside. Only when the key
// doesn't hit does the Field type decide whether to dig into the value (see
// mask_nested.go).
func (m *masker) maskField(f zapcore.Field, depth int) (zapcore.Field, bool) {
	switch f.Type {
	case zapcore.NamespaceType, zapcore.SkipType:
		// A namespace has only a name, no value; replacing it would scramble the
		// nesting of subsequent fields. Fields inside the namespace already go
		// through maskField / the filtering encoder individually.
		return f, false
	}

	if m.hit(f.Key) {
		return zapcore.Field{
			Key:    f.Key,
			Type:   zapcore.StringType,
			String: maskPlaceholder,
		}, true
	}

	switch f.Type {
	case zapcore.InlineMarshalerType:
		// Inline's Field Key is an empty string; the fields get flattened into
		// the current namespace -- there's no key to look up, so we can only
		// wrap it with a filtering encoder and look inside.
		om, ok := f.Interface.(zapcore.ObjectMarshaler)
		if !ok {
			return f, false
		}
		return zapcore.Field{
			Key:       f.Key,
			Type:      zapcore.InlineMarshalerType,
			Interface: &maskObjectMarshaler{om: om, m: m, depth: depth + 1},
		}, true

	case zapcore.ObjectMarshalerType:
		om, ok := f.Interface.(zapcore.ObjectMarshaler)
		if !ok {
			return f, false
		}
		return zapcore.Field{
			Key:       f.Key,
			Type:      zapcore.ObjectMarshalerType,
			Interface: &maskObjectMarshaler{om: om, m: m, depth: depth + 1},
		}, true

	case zapcore.ArrayMarshalerType:
		am, ok := f.Interface.(zapcore.ArrayMarshaler)
		if !ok {
			return f, false
		}
		return zapcore.Field{
			Key:       f.Key,
			Type:      zapcore.ArrayMarshalerType,
			Interface: &maskArrayMarshaler{am: am, m: m, depth: depth + 1},
		}, true

	case zapcore.ReflectType:
		nv, changed := m.maskReflected(f.Interface, depth+1)
		if !changed {
			return f, false
		}
		return zapcore.Field{
			Key:       f.Key,
			Type:      zapcore.ReflectType,
			Interface: nv,
		}, true
	}

	return f, false
}

// ---------------------------------------------------------------------------
// Interception at the Core layer
// ---------------------------------------------------------------------------

// maskCore intercepts sensitive fields at the Core layer.
//
// Correct assembly: wrap each leaf sink with its own maskCore, with maskCore
// inside the Tee.
//
//	zapcore.NewTee(
//	    newMaskCore(consoleCore, m),
//	    newMaskCore(fileCore, m),
//	    newMaskCore(errFileCore, m),
//	)
//
// It must not be wrapped outside the Tee instead -- maskCore.Check would hang
// itself onto the CheckedEntry, so the Tee's per-sink level filtering
// (zapcore/tee.go:74-79) would never run again, and error_path would receive
// the full, unfiltered log stream. The sampler is the opposite case: it wraps
// outside the Tee.
//
// Because maskCore is part of *zap.Logger, the log.Zap() escape hatch is
// covered as well.
//
// The inner core must live in the unexported field inner and must not be
// embedded as zapcore.Core: the implicit field name Core produced by embedding
// is an exported identifier, and reflect's CanInterface() returns true for it
// (regardless of whether the outer type is exported), so any code that gets
// hold of this core can pull out the unmasked inner core in three lines and
// write logs directly through it. The second benefit is that when zapcore.Core
// gains new methods in the future, embedding would silently inherit the inner
// implementation (a new bypass path), whereas an explicit implementation makes
// that a compile error.
type maskCore struct {
	inner zapcore.Core
	m     *masker
}

func newMaskCore(c zapcore.Core, m *masker) zapcore.Core {
	return &maskCore{inner: c, m: m}
}

func (c *maskCore) Enabled(l zapcore.Level) bool { return c.inner.Enabled(l) }

func (c *maskCore) Sync() error { return c.inner.Sync() }

func (c *maskCore) With(fs []zapcore.Field) zapcore.Core {
	return &maskCore{inner: c.inner.With(c.m.apply(fs)), m: c.m}
}

// Check must be overridden. The base Check would hang the inner Core onto the
// CheckedEntry, after which Write goes straight to the inner core and masking
// is bypassed entirely.
func (c *maskCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

func (c *maskCore) Write(ent zapcore.Entry, fs []zapcore.Field) error {
	return c.inner.Write(ent, c.m.apply(fs))
}
