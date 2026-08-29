// Nested masking handles the case where "a sensitive value hides inside a value".
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
//     even reaches the nested-mask traversal. Same category as case 2: the caller
//     explicitly decided the output form. The field key is still checked -- zap.Any("password", stringerValue)
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
// isValidTag in encode.go no longer participate in the build). Alignment is based
// strictly on **measured on-disk bytes**, never on v1 source -- the two differ in
// behavior around map keys and invalid tag names.

package log

import (
	"encoding"
	"encoding/json"
	"reflect"
	"strings"
)

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
