package log

import (
	"encoding"
	"math"
	"reflect"
	"strconv"
)

// maskMap recursively masks a map. A string key goes down the zero-allocation fast
// path; other key types are first stringified per encoding/json's rules and then run
// through the same judgment.
//
// This used to let non-string keys through unconditionally, skipping the value
// recursion along with it -- that was based on the unverified assumption that
// "encoding/json cannot handle maps with non-string keys". It handles them just fine:
// map[int64]Order / map[uint64]User is one of the most common ID-indexed patterns in
// Go, and it is measured to land on disk as
// {"7":{"user":"alice","password":"hunter2"}} -- a whole block of plaintext.
func (m *masker) maskMap(rv reflect.Value, depth int, seen *map[uintptr]struct{}) (any, bool) {
	if rv.IsNil() {
		return nil, false
	}
	kt := rv.Type().Key()
	if kt.Kind() == reflect.String && !kt.Implements(textMarshalerType) {
		return m.maskStringKeyMap(rv, depth, seen)
	}
	if !maskMapKeyEncodable(kt) {
		// encoding/json cannot encode a member name for this kind of key; it reports
		// unsupported value, and zap records the whole field as
		// "<key>Error":"json: unsupported value: ..." -- no plaintext lands on disk
		// (measured for both map[bool]X and map[struct]X). It is safe to just hand
		// it back as-is.
		return nil, false
	}
	return m.maskOtherKeyMap(rv, depth, seen)
}

// maskStringKeyMap is the fast path for string keys: the key needs no conversion, and
// when there is no hit, not a single byte is copied.
func (m *masker) maskStringKeyMap(rv reflect.Value, depth int, seen *map[uintptr]struct{}) (any, bool) {
	var out map[string]any
	snapshot := func() map[string]any {
		// Map iteration order is random, so there is no such thing as an
		// "already-processed prefix"; take a full snapshot on the very first hit.
		dst := make(map[string]any, rv.Len())
		it := rv.MapRange()
		for it.Next() {
			dst[it.Key().String()] = it.Value().Interface()
		}
		return dst
	}

	iter := rv.MapRange()
	for iter.Next() {
		k := iter.Key().String()
		if m.hit(k) {
			if out == nil {
				out = snapshot()
			}
			out[k] = maskPlaceholder
			continue
		}
		nv, ch := m.maskValue(iter.Value(), depth+1, seen)
		if ch {
			if out == nil {
				out = snapshot()
			}
			out[k] = nv
		}
	}

	if out == nil {
		return nil, false
	}
	return out, true
}

// maskOtherKeyMap handles maps with non-string keys, producing a map[string]any.
//
// The shape is unchanged: map[int]X already serializes to {"1":{...}}, and converting
// it to map[string]any{"1":...} still serializes to {"1":{...}}. The zero-copy
// principle still holds -- take a full snapshot only on the first hit.
//
// If any single key fails to stringify, the whole thing is handed back as-is:
// encoding/json treats a map as all-or-nothing -- if one key cannot be encoded into a
// member name, the entire field fails to encode and is recorded as <key>Error. We
// follow suit here, not because the shape must match, but because we simply cannot
// judge that key.
//
// The shape was never guaranteed to match in the first place: for map[any]X{1.5: ...},
// we can encode the key ("1.5"), but encoding/json reports unsupported value (a float
// boxed in an interface does not go through its map-key path), so when masking hits we
// output {"1.5":{"password":"***"}}, while the same unmasked data lands on disk as
// vError. What this function guarantees is that no plaintext leaks -- not that it
// produces the same shape as json.Marshal would on the unmasked data.
func (m *masker) maskOtherKeyMap(rv reflect.Value, depth int, seen *map[uintptr]struct{}) (any, bool) {
	var out map[string]any
	snapshot := func() (map[string]any, bool) {
		dst := make(map[string]any, rv.Len())
		it := rv.MapRange()
		for it.Next() {
			name, ok := maskMapKeyName(it.Key())
			if !ok {
				return nil, false
			}
			dst[name] = it.Value().Interface()
		}
		return dst, true
	}

	iter := rv.MapRange()
	for iter.Next() {
		name, ok := maskMapKeyName(iter.Key())
		if !ok {
			return nil, false
		}
		// Check the stringified key against the blocklist once, uniformly: hit goes
		// down a zero-allocation path, so the cost is negligible, and the payoff is
		// that when a key type's MarshalText produces a meaningful string (say a
		// HeaderName type producing "authorization"), it gets blocked.
		if m.hitMapKey(iter.Key(), name) {
			if out == nil {
				if out, ok = snapshot(); !ok {
					return nil, false
				}
			}
			out[name] = maskPlaceholder
			continue
		}
		nv, ch := m.maskValue(iter.Value(), depth+1, seen)
		if ch {
			if out == nil {
				if out, ok = snapshot(); !ok {
					return nil, false
				}
			}
			out[name] = nv
		}
	}

	if out == nil {
		return nil, false
	}
	return out, true
}

// hitMapKey checks whether a map member name needs masking. Besides the member name
// itself, it also checks one extra case: for a key type whose underlying type is
// string and that also carries its own MarshalText, jsonv2 uses the MarshalText
// result as the member name while jsonv1 uses the raw string -- both names are
// blocked, so that switching toolchains does not open a gap.
func (m *masker) hitMapKey(k reflect.Value, name string) bool {
	if m.hit(name) {
		return true
	}
	if k.Kind() == reflect.Interface && !k.IsNil() {
		k = k.Elem()
	}
	if k.Kind() != reflect.String {
		return false
	}
	raw := k.String()
	return raw != name && m.hit(raw)
}

// maskMapKeyEncodable reports whether this kind of key can possibly be encoded into a
// JSON member name. This only does a type-level screen; whether a specific key can
// actually be encoded (NaN, MarshalText returning an error, a bool boxed in
// map[any]V) is judged individually by maskMapKeyName.
func maskMapKeyEncodable(t reflect.Type) bool {
	if t.Implements(textMarshalerType) {
		return true
	}
	switch t.Kind() {
	case reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Uintptr, reflect.Float32, reflect.Float64,
		reflect.Interface: // map[any]V is legal; {1:...} is measured to land on disk as {"1":...}
		return true
	}
	return false
}

// maskMapKeyName converts a map key into the member name it would have in a JSON
// object; ok=false means it cannot be encoded.
//
// The rule is based on Go 1.27's measured on-disk bytes (at this point encoding/json
// is implemented by json/v2; v1's resolveKeyName no longer participates in the build,
// and the two are not equivalent):
//
//   - an interface is unboxed first, then judged again by its dynamic type;
//   - a type implementing encoding.TextMarshaler uses the MarshalText result,
//     **taking priority over Kind** -- v2 goes through MarshalText even for a type
//     whose underlying type is string, whereas v1 prefers string first. The
//     divergence only affects how the member name is written; when checking the
//     blocklist, the raw string is also checked separately below;
//   - integers use decimal, floats use the JSON number format;
//   - the key type's own MarshalJSON **does not count**: v2 is measured to not call
//     it at the member-name position; what lands on disk is the underlying numeric
//     value.
func maskMapKeyName(k reflect.Value) (string, bool) {
	if k.Kind() == reflect.Interface {
		if k.IsNil() {
			return "", false
		}
		return maskMapKeyName(k.Elem())
	}
	if k.Type().Implements(textMarshalerType) {
		if k.Kind() == reflect.Pointer && k.IsNil() {
			return "", true // both v1 and v2 write it as an empty member name
		}
		if !k.CanInterface() {
			return "", false
		}
		tm, ok := k.Interface().(encoding.TextMarshaler)
		if !ok {
			return "", false
		}
		b, err := tm.MarshalText()
		if err != nil {
			return "", false // json fails the same way; nothing lands on disk
		}
		return string(b), true
	}
	switch k.Kind() {
	case reflect.String:
		return k.String(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(k.Int(), 10), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32,
		reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(k.Uint(), 10), true
	case reflect.Float32:
		return maskFormatFloat(k.Float(), 32)
	case reflect.Float64:
		return maskFormatFloat(k.Float(), 64)
	}
	return "", false
}

// maskFormatFloat replicates encoding/json's actual format for writing a JSON number,
// which is to say internal/jsonwire.AppendFloat: use 'f' when |x| falls in
// [1e-6, 1e21), otherwise use 'e', then collapse e-09 down to e-9.
//
// The alignment target is **encoding/json's actual behavior**, not the ECMAScript
// spec text -- AppendFloat is largely modeled on Number::toString, but it diverges
// from the spec at least on -0: the spec calls for writing 0, while both json and this
// function write -0. We follow json, not the spec.
//
// Using strconv's 'g' directly would write 1e+20 around the 1e20 mark, while json
// writes 100000000000000000000 -- the member names would not match and log search
// rules would break. NaN / +-Inf cannot be encoded as a JSON number; json reports
// unsupported value, so this returns ok=false.
func maskFormatFloat(f float64, bits int) (string, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", false
	}
	if bits == 32 {
		f = float64(float32(f))
	}
	abs := math.Abs(f)
	format := byte('f')
	if abs != 0 {
		if bits == 64 && (abs < 1e-6 || abs >= 1e21) ||
			bits == 32 && (float32(abs) < 1e-6 || float32(abs) >= 1e21) {
			format = 'e'
		}
	}
	b := strconv.AppendFloat(make([]byte, 0, 32), f, format, -1, bits)
	if format == 'e' {
		if n := len(b); n >= 4 && b[n-4] == 'e' && b[n-3] == '-' && b[n-2] == '0' {
			b[n-2] = b[n-1]
			b = b[:n-1]
		}
	}
	return string(b), true
}
