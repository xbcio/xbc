// mask_nested.go handles the case where "a sensitive value hides inside a value".
//
// mask.go's key blocklist only looks at Field.Key, but zap has four kinds of Field
// that put the real data inside the value: Inline / Object / Array marshalers, and
// Reflect, which goes through reflection-based serialization. Of these, zap.Inline
// is the most dangerous -- the Field it produces has an empty Key, and AddTo flattens
// the object's fields into the top-level namespace, so the final output is byte-for-byte
// indistinguishable from zap.String("password", ...), yet it never goes through any
// key check.
//
// The countermeasure has two parts: marshaler-typed fields get wrapped in a filtering
// encoder (forwards all writes, but writes *** instead of the real value for keys that
// hit the blocklist); Reflect-typed values get one pass of reflective traversal.
//
// # What this line of defense does not cover
//
// The following four cases are outside the scope of this defense; readers of this code
// should not assume it is exhaustive:
//
//  1. A sensitive value written into Entry.Message (log.L().Info("password=" + pwd)) --
//     the field-level blocklist has no control over the message body.
//  2. A type that implements its own MarshalJSON / MarshalText and outputs sensitive
//     content there -- the reflective traversal leaves such types alone as-is (otherwise
//     time.Time would be torn apart into its three unexported fields wall/ext/loc and the
//     output would be completely wrecked). That is a serialization the caller explicitly
//     customized, and it is the caller's responsibility. Note that only these two
//     interfaces count -- fmt.Stringer does not; see hasSelfMarshal for why.
//  3. A subtree deeper than maxMaskDepth is replaced wholesale with ***. This is
//     intentional loss of information, meant to defend against malicious or pathologically
//     deep nesting, not a performance optimization. maxMaskDepth counts structural nesting
//     levels; pointer dereferences and interface unboxing do not count toward it.
//  4. A fmt.Stringer / error passed directly to zap.Any -- in zap.Any's type switch these
//     two branches come before Reflect, so the value goes through StringerType /
//     ErrorType and what lands on disk is the result of String() / Error(), which never
//     even reaches this file. Same category as case 2: the caller explicitly decided the
//     output form. The field key is still checked -- zap.Any("password", stringerValue)
//     is still caught; what cannot be caught is zap.Any("creds", v) where v.String()
//     itself spits out the secret.
//
// Anonymous embedded fields are flattened into the parent level following encoding/json's
// rules (no json name + one pointer dereference yields a struct => flatten), with the
// outer level winning on conflict. This must stay aligned with json: the shape only
// changes when a hit occurs, which is exactly when log shape stability matters most --
// {"Base":{"password":"***"}} would break search rules targeting .password. Multi-path
// conflicts at the same depth do not get the full disambiguation that encoding/json does;
// whichever embedding appears first wins.
//
// # Notes on staying aligned with encoding/json
//
// Starting with Go 1.27, GOEXPERIMENT enables jsonv2 by default, and encoding/json's
// implementation is replaced by encoding/json/v2 + jsontext (newMapEncoder / typeFields /
// isValidTag in encode.go no longer participate in the build). This file's alignment is
// based strictly on **measured on-disk bytes**, never on v1 source -- the two differ in
// behavior around map keys and invalid tag names.

package log

import (
	"encoding"
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap/zapcore"
)

// maxMaskDepth is the upper bound on nesting depth for masking; once exceeded, the
// whole subtree is written out as maskPlaceholder.
const maxMaskDepth = 8

// ---------------------------------------------------------------------------
// marshaler wrapping
// ---------------------------------------------------------------------------

// maskObjectMarshaler routes the inner marshaler's writes through the filtering encoder.
type maskObjectMarshaler struct {
	om    zapcore.ObjectMarshaler
	m     *masker
	depth int
}

func (w *maskObjectMarshaler) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	return w.om.MarshalLogObject(&maskObjectEncoder{enc: enc, m: w.m, depth: w.depth})
}

// maskArrayMarshaler is the same idea, for arrays.
type maskArrayMarshaler struct {
	am    zapcore.ArrayMarshaler
	m     *masker
	depth int
}

func (w *maskArrayMarshaler) MarshalLogArray(enc zapcore.ArrayEncoder) error {
	return w.am.MarshalLogArray(&maskArrayEncoder{enc: enc, m: w.m, depth: w.depth})
}

// ---------------------------------------------------------------------------
// filtering encoders
// ---------------------------------------------------------------------------

// maskObjectEncoder forwards all writes, but writes *** instead of the real value for
// keys that hit the blocklist.
//
// It uses a named field enc rather than embedding zapcore.ObjectEncoder: embedding would
// mean that if zap adds a method to the interface in the future, the new method would be
// silently forwarded to the inner encoder -- a bypass path that would never fail to
// compile. Implementing every method explicitly makes it a compile error instead,
// forcing someone to come fill it in.
type maskObjectEncoder struct {
	enc   zapcore.ObjectEncoder
	m     *masker
	depth int
}

var _ zapcore.ObjectEncoder = (*maskObjectEncoder)(nil)

// AddArray: replace the whole value on a key hit, otherwise recursively wrap and go
// one level deeper.
func (e *maskObjectEncoder) AddArray(k string, v zapcore.ArrayMarshaler) error {
	if e.m.hit(k) || e.depth >= maxMaskDepth {
		e.enc.AddString(k, maskPlaceholder)
		return nil
	}
	return e.enc.AddArray(k, &maskArrayMarshaler{am: v, m: e.m, depth: e.depth + 1})
}

// AddObject: replace the whole value on a key hit, otherwise recursively wrap and go
// one level deeper.
func (e *maskObjectEncoder) AddObject(k string, v zapcore.ObjectMarshaler) error {
	if e.m.hit(k) || e.depth >= maxMaskDepth {
		e.enc.AddString(k, maskPlaceholder)
		return nil
	}
	return e.enc.AddObject(k, &maskObjectMarshaler{om: v, m: e.m, depth: e.depth + 1})
}

// AddReflected: replace the whole value on a key hit, otherwise run the value through
// reflective traversal first.
func (e *maskObjectEncoder) AddReflected(k string, v any) error {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return nil
	}
	if nv, changed := e.m.maskReflected(v, e.depth+1); changed {
		return e.enc.AddReflected(k, nv)
	}
	return e.enc.AddReflected(k, v)
}

// OpenNamespace forwards as-is: keys inside the namespace still go through this
// encoder's Add* methods, so they are naturally covered without any extra handling.
func (e *maskObjectEncoder) OpenNamespace(k string) { e.enc.OpenNamespace(k) }

func (e *maskObjectEncoder) AddBinary(k string, v []byte) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddBinary(k, v)
}

func (e *maskObjectEncoder) AddByteString(k string, v []byte) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddByteString(k, v)
}

func (e *maskObjectEncoder) AddBool(k string, v bool) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddBool(k, v)
}

func (e *maskObjectEncoder) AddComplex128(k string, v complex128) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddComplex128(k, v)
}

func (e *maskObjectEncoder) AddComplex64(k string, v complex64) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddComplex64(k, v)
}

func (e *maskObjectEncoder) AddDuration(k string, v time.Duration) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddDuration(k, v)
}

func (e *maskObjectEncoder) AddFloat64(k string, v float64) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddFloat64(k, v)
}

func (e *maskObjectEncoder) AddFloat32(k string, v float32) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddFloat32(k, v)
}

func (e *maskObjectEncoder) AddInt(k string, v int) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddInt(k, v)
}

func (e *maskObjectEncoder) AddInt64(k string, v int64) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddInt64(k, v)
}

func (e *maskObjectEncoder) AddInt32(k string, v int32) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddInt32(k, v)
}

func (e *maskObjectEncoder) AddInt16(k string, v int16) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddInt16(k, v)
}

func (e *maskObjectEncoder) AddInt8(k string, v int8) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddInt8(k, v)
}

func (e *maskObjectEncoder) AddString(k, v string) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddString(k, v)
}

func (e *maskObjectEncoder) AddTime(k string, v time.Time) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddTime(k, v)
}

func (e *maskObjectEncoder) AddUint(k string, v uint) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddUint(k, v)
}

func (e *maskObjectEncoder) AddUint64(k string, v uint64) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddUint64(k, v)
}

func (e *maskObjectEncoder) AddUint32(k string, v uint32) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddUint32(k, v)
}

func (e *maskObjectEncoder) AddUint16(k string, v uint16) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddUint16(k, v)
}

func (e *maskObjectEncoder) AddUint8(k string, v uint8) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddUint8(k, v)
}

func (e *maskObjectEncoder) AddUintptr(k string, v uintptr) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddUintptr(k, v)
}

// maskArrayEncoder is the array counterpart. Array elements have no key, so scalars
// are forwarded directly; only the three methods that can nest another layer of
// structure (Object / Array / Reflected) need filtering.
type maskArrayEncoder struct {
	enc   zapcore.ArrayEncoder
	m     *masker
	depth int
}

var _ zapcore.ArrayEncoder = (*maskArrayEncoder)(nil)

func (e *maskArrayEncoder) AppendArray(v zapcore.ArrayMarshaler) error {
	if e.depth >= maxMaskDepth {
		e.enc.AppendString(maskPlaceholder)
		return nil
	}
	return e.enc.AppendArray(&maskArrayMarshaler{am: v, m: e.m, depth: e.depth + 1})
}

func (e *maskArrayEncoder) AppendObject(v zapcore.ObjectMarshaler) error {
	if e.depth >= maxMaskDepth {
		e.enc.AppendString(maskPlaceholder)
		return nil
	}
	return e.enc.AppendObject(&maskObjectMarshaler{om: v, m: e.m, depth: e.depth + 1})
}

func (e *maskArrayEncoder) AppendReflected(v any) error {
	if nv, changed := e.m.maskReflected(v, e.depth+1); changed {
		return e.enc.AppendReflected(nv)
	}
	return e.enc.AppendReflected(v)
}

func (e *maskArrayEncoder) AppendBool(v bool)             { e.enc.AppendBool(v) }
func (e *maskArrayEncoder) AppendByteString(v []byte)     { e.enc.AppendByteString(v) }
func (e *maskArrayEncoder) AppendComplex128(v complex128) { e.enc.AppendComplex128(v) }
func (e *maskArrayEncoder) AppendComplex64(v complex64)   { e.enc.AppendComplex64(v) }
func (e *maskArrayEncoder) AppendDuration(v time.Duration) {
	e.enc.AppendDuration(v)
}
func (e *maskArrayEncoder) AppendFloat64(v float64) { e.enc.AppendFloat64(v) }
func (e *maskArrayEncoder) AppendFloat32(v float32) { e.enc.AppendFloat32(v) }
func (e *maskArrayEncoder) AppendInt(v int)         { e.enc.AppendInt(v) }
func (e *maskArrayEncoder) AppendInt64(v int64)     { e.enc.AppendInt64(v) }
func (e *maskArrayEncoder) AppendInt32(v int32)     { e.enc.AppendInt32(v) }
func (e *maskArrayEncoder) AppendInt16(v int16)     { e.enc.AppendInt16(v) }
func (e *maskArrayEncoder) AppendInt8(v int8)       { e.enc.AppendInt8(v) }
func (e *maskArrayEncoder) AppendString(v string)   { e.enc.AppendString(v) }
func (e *maskArrayEncoder) AppendTime(v time.Time)  { e.enc.AppendTime(v) }
func (e *maskArrayEncoder) AppendUint(v uint)       { e.enc.AppendUint(v) }
func (e *maskArrayEncoder) AppendUint64(v uint64)   { e.enc.AppendUint64(v) }
func (e *maskArrayEncoder) AppendUint32(v uint32)   { e.enc.AppendUint32(v) }
func (e *maskArrayEncoder) AppendUint16(v uint16)   { e.enc.AppendUint16(v) }
func (e *maskArrayEncoder) AppendUint8(v uint8)     { e.enc.AppendUint8(v) }
func (e *maskArrayEncoder) AppendUintptr(v uintptr) { e.enc.AppendUintptr(v) }

// ---------------------------------------------------------------------------
// reflective traversal
// ---------------------------------------------------------------------------

var (
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

// hasSelfMarshal reports whether a type carries its own serialization. Such types must
// be left as-is -- otherwise time.Time would be torn apart into its three unexported
// fields wall/ext/loc and the output would be completely wrecked.
//
// It only checks the two interfaces json.Marshaler and encoding.TextMarshaler --
// **it deliberately does not check fmt.Stringer**. This is not an oversight to be
// "fixed": zap encodes ReflectType via json, and encoding/json only recognizes these
// two interfaces, never looking at String() at all. Judging a type that only implements
// String() to be "self-serializing" would mean actively skipping traversal on it, and
// json would then flatten it field by field anyway -- a pure leak with no compensating
// benefit. time.Time is blocked by MarshalJSON / MarshalText, net.IP and uuid.UUID go
// through MarshalText, and time.Duration / zapcore.Level are scalar Kinds that never
// even reach the struct branch.
//
// Likewise it only checks the type's own method set, not reflect.PointerTo(t). Methods
// with a pointer receiver are not in the value type's method set, and encoding/json
// also never calls them on an unaddressable value -- if this check recognized them
// anyway, a type that only implements MarshalJSON on a pointer receiver would be judged
// "self-serializing" and skip traversal when logged by value, forming the same kind of
// bypass path.
func hasSelfMarshal(t reflect.Type) bool {
	if t == nil {
		return false
	}
	return t.Implements(jsonMarshalerType) || t.Implements(textMarshalerType)
}

// maskReflected recursively masks an arbitrary value. When changed is false, the
// caller must use the original value, not the return value.
//
// When there is no hit, not a single byte is copied: the recursion returns
// changed=false all the way up and the original value is handed back as-is. Most
// structs contain no sensitive fields, so this path must be zero-overhead -- the same
// design as apply's copy-on-first-hit.
func (m *masker) maskReflected(v any, depth int) (out any, changed bool) {
	if v == nil {
		return v, false
	}
	var seen map[uintptr]struct{}
	nv, ch := m.maskValue(reflect.ValueOf(v), depth, &seen)
	if !ch {
		return v, false
	}
	return nv, true
}

// maskValue is the recursive body of maskReflected. seen records the pointer addresses
// visited along the recursion path; it is only allocated once recursion actually
// reaches a pointer, and stays nil when there are no nested pointers.
//
// depth only counts **structural nesting levels**: pointer dereferences and interface
// unboxing are indirection layers, not nesting layers, and do not increment depth.
// Otherwise the actual meaning of maxMaskDepth would drift with how a value happens to
// be represented -- a map[string]any would burn 2 units of budget per level
// (Interface + Map), leaving only 4 usable levels out of 8. Not incrementing is safe:
// circular references are independently blocked by seen and never rely on depth as a
// backstop; interface boxing must eventually box a concrete type, so there is no such
// thing as infinite indirection through it.
func (m *masker) maskValue(rv reflect.Value, depth int, seen *map[uintptr]struct{}) (any, bool) {
	if !rv.IsValid() {
		return nil, false
	}

	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Struct, reflect.Map,
		reflect.Slice, reflect.Array:
	default:
		// Scalars, chan, func, etc. have no inner structure to descend into.
		return nil, false
	}

	if hasSelfMarshal(rv.Type()) {
		return nil, false
	}
	if depth > maxMaskDepth {
		return maskPlaceholder, true
	}

	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return nil, false
		}
		addr := rv.Pointer()
		if *seen == nil {
			*seen = make(map[uintptr]struct{}, 4)
		}
		if _, dup := (*seen)[addr]; dup {
			return maskPlaceholder, true
		}
		(*seen)[addr] = struct{}{}
		nv, ch := m.maskValue(rv.Elem(), depth, seen)
		delete(*seen, addr)
		return nv, ch

	case reflect.Interface:
		if rv.IsNil() {
			return nil, false
		}
		return m.maskValue(rv.Elem(), depth, seen)

	case reflect.Struct:
		return m.maskStruct(rv, depth, seen)

	case reflect.Map:
		return m.maskMap(rv, depth, seen)

	case reflect.Slice, reflect.Array:
		return m.maskSlice(rv, depth, seen)
	}

	return nil, false
}

// maskStruct walks the exported fields. A field's name is the name portion of its
// json tag, or the Go field name if there is no tag. A struct with no exported fields
// at all is returned as-is (the general fallback beyond the time.Time special case).
//
// Two passes: first the named fields at this level, then the flattened anonymous
// embedded fields (merging never overwrites an existing key). The order cannot be
// reversed -- this is exactly what "outer wins" means, consistent with encoding/json's
// shallower-wins rule.
func (m *masker) maskStruct(rv reflect.Value, depth int, seen *map[uintptr]struct{}) (any, bool) {
	t := rv.Type()
	n := t.NumField()
	var out map[string]any

	// First pass: named fields at this level.
	for i := range n {
		sf := t.Field(i)
		if !maskNamedField(sf) {
			continue
		}
		name := maskFieldName(sf)
		if name == "" { // json:"-"
			continue
		}
		fv := rv.Field(i)

		if m.hitFieldName(sf, name) {
			if out == nil {
				out = maskNamedRaw(rv)
			}
			out[name] = maskPlaceholder
			continue
		}
		if !fv.CanInterface() {
			// An unexported anonymous struct embed (its json tag gave it a name, so
			// it is not flattened). It is the only kind of field that "json includes
			// but reflect refuses to hand back via Interface()", so it unconditionally
			// goes down the rebuild path -- the "return as-is" path below requires
			// Interface(), which is not available here.
			if out == nil {
				out = maskNamedRaw(rv)
			}
			out[name] = m.maskUnexported(fv, depth+1, seen)
			continue
		}
		nv, ch := m.maskValue(fv, depth+1, seen)
		if ch {
			if out == nil {
				out = maskNamedRaw(rv)
			}
			out[name] = nv
		}
	}

	// Second pass: flattened anonymous embedded fields.
	for i := range n {
		sf := t.Field(i)
		if !maskEmbedFlatten(sf) {
			continue
		}
		fv := rv.Field(i)
		if fv.Kind() == reflect.Pointer && fv.IsNil() {
			continue // nil embedded pointer: encoding/json likewise produces no key for it
		}

		nv, ch := m.maskEmbedStruct(fv, depth, seen)
		if !ch {
			if out != nil {
				maskStructRaw(out, fv, maxMaskDepth)
			}
			continue
		}
		if out == nil {
			out = maskNamedRaw(rv)
			for j := range i { // backfill the embedded fields before this one, as-is
				if maskEmbedFlatten(t.Field(j)) {
					maskStructRaw(out, rv.Field(j), maxMaskDepth)
				}
			}
		}
		if sub, isMap := nv.(map[string]any); isMap {
			for k, v := range sub {
				if _, dup := out[k]; !dup {
					out[k] = v
				}
			}
			continue
		}
		// Degenerate case such as a circular reference: cannot be flattened, so
		// attach it under the type name.
		if _, dup := out[sf.Name]; !dup {
			out[sf.Name] = nv
		}
	}

	if out == nil {
		return nil, false
	}
	return out, true
}

// maskNamedField reports whether a field is treated as a "named field at this level",
// with the rule aligned to encoding/json (measured against Go 1.27's on-disk bytes;
// v1's typeFields comment says the same thing):
//
//   - A flattened anonymous embed does not count as a named field; it goes through the
//     second pass instead;
//   - Every exported field counts;
//   - Among unexported fields, only the kind that is **anonymous and, after one pointer
//     dereference, a struct** counts -- json's skip condition is "unexported **and**
//     not a struct type", because an unexported struct type can still have exported
//     fields of its own. type S struct{ sHidden `json:"base"` } falls into this case;
//     json.Marshal is measured to produce {"base":{"password":"..."}}.
//
// This used to unconditionally skip all unexported fields, which was stricter than
// json -- and being stricter here means leaking: json still puts the password inside
// on disk, while we simply never looked at it.
func maskNamedField(sf reflect.StructField) bool {
	if maskEmbedFlatten(sf) {
		return false
	}
	if sf.IsExported() {
		return true
	}
	if !sf.Anonymous {
		return false // an unexported named field; json ignores it outright
	}
	t := sf.Type
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct
}

// hitFieldName checks the blocklist using **three candidate names**; a hit on any one
// triggers masking.
//
// # Criterion
//
// Criterion B: **block whenever a sensitive core word appears anywhere in the tag
// text**.
//
// This is not criterion A ("mask only when the on-disk member name hits the
// blocklist"). The difference between the two is clearest on `json:"db.password\"x"`:
// its actual on-disk member name is db (v2) or Foo (v1), and neither is sensitive at
// all -- under criterion A it should not be masked, yet we mask it anyway. The
// replacement-name candidate below does not correspond to any real on-disk name; only
// criterion B explains why it exists.
//
// Why criterion B is the right one: a malformed tag does not make the value any less
// sensitive. A field like Password string `json:"db\"password"` still holds a
// plaintext password, whether the on-disk member name ends up being db or Foo does not
// change that. The severity level and the trade-off follow the criterion, not the
// probability of triggering it.
//
// # Three candidates
//
// The first two cover the two normal cases -- a well-formed tag, and one that fails to
// parse -- and go through hit's **suffix word-group** rule:
//
//  1. the name portion of the tag (also the member name we emit in output)
//  2. the Go field name -- used when v1 validation fails, and also used by v2 when the
//     tag's first character is illegal
//
// The third covers the case where the tag is corrupted by reserved characters
// (backslash, single quote, double quote, backtick):
//
//  3. the replacement name: replace every \ ' " ` and comma in the **entire tag** with
//     `_`, and go through hitWindow's **word-window** rule
//
// # Why the third candidate uses a word window instead of piling on more
// position-based candidates
//
// The text shapes of malformed tags are unbounded. An earlier version added
// candidates one at a time based on "where the garbage sits relative to the sensitive
// core word" (before -> replacement name, after -> truncated name, after -> v2 name),
// and on that basis claimed there were only three possible positions -- before,
// middle, after -- so the candidate set was closed. **That closure claim was wrong**:
// garbage can appear on both sides at once. `json:"db\password\x"` normalizes to
// db / password / x, with the sensitive word sandwiched in the middle, and none of the
// three directional candidates can reach it -- the plaintext password lands on disk
// as-is.
//
// A word window is position-independent: once every reserved character in the whole
// tag is replaced with a separator, the sensitive core word becomes a **complete,
// contiguous word window** no matter which segment it falls into. All four positions
// are covered in one shot, with no need to enumerate a fifth.
//
//	before   db"password           -> db / password
//	middle   pass"word             -> pass / word
//	after    db.password"x         -> db / password / x
//	both     svc\secret_key\extra  -> svc / secret / key / extra
//
// The word window also **subsumes** the two candidates it replaces -- the truncated
// name and the v2 name -- so dropping them loses no coverage: both are prefixes of the
// tag, and both terminate at a reserved character or `-` `.` -- after replacement that
// position is necessarily a word boundary, so every suffix word group of either one is
// also some word window of the replacement name. This is not a guess: degrading
// hitWindow to plain suffix matching makes the 5 tests that originally pinned down
// these two candidates fail, along with the newly added pincer-case tests.
//
// # Blast radius
//
// Window matching is looser than suffix matching, but it **only applies to tags that
// contain reserved characters**; normal field names always go through the suffix rule
// of candidates 1 and 2, so the extra leniency does not leak out. Existing verdicts are
// therefore unaffected: TokenCount int `json:"n"`'s suffix word groups are
// count / tokencount, which do not hit; `json:"count-extra\y"`'s replacement name
// count-extra_y, and `json:"tokenizer\"x"`'s tokenizer_x, all have word windows that do
// not hit either.
//
// The cost is that the verdict on pathological tags becomes more conservative:
// `json:"phone\"masked"` gets masked because of the window phone, even though
// phone_masked itself has an established "no hit" verdict. On a corrupted tag, we
// would rather over-mask than under-mask.
func (m *masker) hitFieldName(sf reflect.StructField, name string) bool {
	if m.hit(name) {
		return true
	}
	if name != sf.Name && m.hit(sf.Name) {
		return true
	}
	// Only build the replacement name when the tag actually contains a reserved
	// character, to avoid a pointless string allocation for the vast majority of
	// normal fields. maskTagReserved's first character is the comma: the comma alone
	// does not trigger this path (`,omitempty` is a perfectly normal thing to write),
	// but once some other reserved character has triggered this path, the comma also
	// gets replaced with a separator, so that something like
	// `json:"db,omitempty\"password"` can be split apart too.
	tag := sf.Tag.Get("json")
	return strings.ContainsAny(tag, maskTagReserved[1:]) &&
		m.hitWindow(maskTagReplacer.Replace(tag))
}

// maskTagReserved is the set of characters encoding/json v2 reserves in the name
// portion of a tag, copied byte-for-byte from parseFieldOptions in
// $GOROOT/src/encoding/json/v2/fields.go.
const maskTagReserved = ",\\'\"`"

// maskTagReplacer replaces reserved characters in a tag with a separator, for use by
// the replacement-name candidate. Reused at package level -- do not construct one on
// every call.
//
// The target character must be a separator that splitMaskKey recognizes (`_`), not the
// empty string: deleting the reserved character would fuse db"password into a
// **single** word dbpassword, and the word window could no longer reach password.
var maskTagReplacer = strings.NewReplacer("\\", "_", "'", "_", "\"", "_", "`", "_", ",", "_")

// maskUnexported handles field values that Interface() cannot reach -- that is,
// unexported anonymous struct embeds (the kind that carries a json name tag and is
// therefore not flattened).
//
// reflect's read-only flag is only added at this one level: flagEmbedRO is not passed
// down to its exported subfields (this is exactly why promoted methods can still be
// called), so rebuilding a map field by field to match json's shape is enough -- no
// need for unsafe. Reaching into an unexported field via unsafe.Pointer in
// security-critical code would cost more than it is worth.
//
// The cost is that the shape is now fixed as a field-by-field expanded object: this
// kind of field always goes down the rebuild path, even when there is not a single
// sensitive field anywhere in the subtree. If the embedded type carries its own
// MarshalJSON, encoding/json would use its custom form, but we cannot (the method
// cannot be called on a read-only value), so we can only expand field by field -- this
// is the one and only shape deviation between this function and json, and it only
// occurs for the single combination of "unexported + anonymous + carries a json name
// tag + self-serializing".
func (m *masker) maskUnexported(fv reflect.Value, depth int, seen *map[uintptr]struct{}) any {
	if depth > maxMaskDepth {
		return maskPlaceholder
	}
	if fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			return nil // json outputs null
		}
		// type a struct{ *a `json:"base"` } is legal; the self-reference must be
		// blocked.
		addr := fv.Pointer()
		if *seen == nil {
			*seen = make(map[uintptr]struct{}, 4)
		}
		if _, dup := (*seen)[addr]; dup {
			return maskPlaceholder
		}
		(*seen)[addr] = struct{}{}
		defer delete(*seen, addr)
		fv = fv.Elem()
	}
	if fv.Kind() != reflect.Struct {
		return maskPlaceholder // maskNamedField guarantees this is unreachable
	}
	if nv, ch := m.maskStruct(fv, depth, seen); ch {
		return nv
	}
	// maskStruct says nothing in the whole subtree hit, so rebuilding it as-is is safe.
	out := make(map[string]any, fv.NumField())
	maskStructRaw(out, fv, maxMaskDepth)
	return out
}

// maskEmbedFlatten reports whether a field is flattened into the parent level under
// encoding/json's rules: an anonymous embed, no name given in the json tag, and
// (after one pointer dereference) a struct.
//
// If the json tag gives it a name (including json:"-"), it is treated as an ordinary
// named field; an embed of a non-struct type (type Req struct{ MyInt }) is also
// treated as an ordinary named field, keyed by the type name. An unexported embedded
// type does not stop flattening -- encoding/json still promotes its exported fields,
// and reflect's CanInterface on those fields is also true; failing to flatten along
// with it would let sensitive fields inside slip through.
func maskEmbedFlatten(sf reflect.StructField) bool {
	if !sf.Anonymous {
		return false
	}
	if tag, ok := sf.Tag.Lookup("json"); ok {
		if name, _, _ := strings.Cut(tag, ","); name != "" {
			return false
		}
	}
	t := sf.Type
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct
}

// maskEmbedStruct expands a flattened embedded field; depth does not increment --
// after flattening, the field sits at the same structural level as its parent.
//
// It goes straight to maskStruct rather than through maskValue: when encoding/json
// decides whether to flatten, it only looks at the type's shape, not whether the
// embedded type itself has a MarshalJSON -- that method is either promoted to the
// outer type, in which case it would block the entire outer level right at
// maskValue's entry point, or it is never called at all due to multi-path ambiguity.
// Going through maskValue would get it caught by hasSelfMarshal, which does not match
// json's actual output.
func (m *masker) maskEmbedStruct(fv reflect.Value, depth int, seen *map[uintptr]struct{}) (any, bool) {
	if fv.Kind() != reflect.Pointer {
		return m.maskStruct(fv, depth, seen)
	}
	// type A struct{ *A } is legal; the self-reference must be blocked.
	addr := fv.Pointer()
	if *seen == nil {
		*seen = make(map[uintptr]struct{}, 4)
	}
	if _, dup := (*seen)[addr]; dup {
		return maskPlaceholder, true
	}
	(*seen)[addr] = struct{}{}
	nv, ch := m.maskStruct(fv.Elem(), depth, seen)
	delete(*seen, addr)
	return nv, ch
}

// maskNamedRaw builds the map only on the first hit, copying this level's named fields
// (not including flattened embeds) across as-is. It is only called on the first hit --
// no field has been rewritten before this point, so copying the whole thing as-is is
// safe.
func maskNamedRaw(rv reflect.Value) map[string]any {
	t := rv.Type()
	n := t.NumField()
	out := make(map[string]any, n)
	for i := range n {
		sf := t.Field(i)
		if !maskNamedField(sf) {
			continue
		}
		name := maskFieldName(sf)
		if name == "" {
			continue
		}
		fv := rv.Field(i)
		if !fv.CanInterface() {
			// Unexported anonymous struct embed. Its unmasked, as-is value is not
			// materialized here in advance -- maskStruct unconditionally goes down
			// the rebuild path for this kind of field, so it will always fill it in.
			continue
		}
		out[name] = fv.Interface()
	}
	return out
}

// maskStructRaw writes rv into out as-is, in its flattened shape: named fields first,
// then embedded fields, never overwriting an existing key. budget is only a
// backstop -- a genuine self-referential embed is judged changed by seen inside
// maskEmbedStruct and never reaches this as-is path in the first place; correctness
// here does not depend on budget.
//
// This is only reached when "nothing in the whole subtree hit" (the caller got
// changed==false), so copying things across as-is never materializes any sensitive
// value.
func maskStructRaw(out map[string]any, rv reflect.Value, budget int) {
	if budget <= 0 {
		return
	}
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return
	}
	t := rv.Type()
	n := t.NumField()
	for i := range n {
		sf := t.Field(i)
		if !maskNamedField(sf) {
			continue
		}
		name := maskFieldName(sf)
		if name == "" {
			continue
		}
		if _, dup := out[name]; dup {
			continue
		}
		fv := rv.Field(i)
		if !fv.CanInterface() {
			// Unexported anonymous struct embed. The precondition above --
			// "nothing in the subtree hit" -- guarantees this is never reached:
			// maskStruct unconditionally sets changed=true for this kind of field,
			// so the caller would never pick the as-is path. Even if this were
			// somehow reached, it would only mean one missing key, never a panic,
			// and certainly never a leak.
			continue
		}
		out[name] = fv.Interface()
	}
	for i := range n {
		if maskEmbedFlatten(t.Field(i)) {
			maskStructRaw(out, rv.Field(i), budget-1)
		}
	}
}

// maskFieldName returns a field's name as it appears in the log: the name portion of
// the json tag takes priority, otherwise the Go field name. An empty return means the
// field is excluded by json:"-".
func maskFieldName(sf reflect.StructField) string {
	tag, ok := sf.Tag.Lookup("json")
	if !ok {
		return sf.Name
	}
	if tag == "-" {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return sf.Name
	}
	return name
}

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

// maskSlice recursively masks each element. []byte is returned as-is, otherwise it
// would be rendered as a string of numbers.
func (m *masker) maskSlice(rv reflect.Value, depth int, seen *map[uintptr]struct{}) (any, bool) {
	if rv.Type().Elem().Kind() == reflect.Uint8 {
		return nil, false
	}
	if rv.Kind() == reflect.Slice && rv.IsNil() {
		return nil, false
	}

	n := rv.Len()
	var out []any
	for i := range n {
		nv, ch := m.maskValue(rv.Index(i), depth+1, seen)
		if ch {
			if out == nil {
				out = make([]any, n)
				for j := range i {
					out[j] = rv.Index(j).Interface()
				}
			}
			out[i] = nv
			continue
		}
		if out != nil {
			out[i] = rv.Index(i).Interface()
		}
	}

	if out == nil {
		return nil, false
	}
	return out, true
}
