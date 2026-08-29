package plugin

import (
	"fmt"
	"reflect"
	"strings"
)

// NotFoundError is returned when a lookup finds nothing for (Want,
// Instance). Closest is nil for a concrete Want (there is no "closest"
// concept when the match is exact-or-nothing) and also nil when the
// instance has nothing registered at all. Populating Closest/Missing with
// "which registered type is nearest and what does it lack" is the RuntimeHost
// implementation's job (package assembly's registry) -- plugin only
// defines the shape the diagnostic travels in and renders it.
type NotFoundError struct {
	Want     reflect.Type
	Instance string
	Closest  reflect.Type
	Missing  []string
}

func (e *NotFoundError) Error() string {
	if e.Closest == nil {
		return fmt.Sprintf("xbc: type %s (instance %q) not found, no type is registered under this instance", e.Want, e.Instance)
	}
	return fmt.Sprintf("xbc: type %s (instance %q) not found, closest is %s, missing methods: %s",
		e.Want, e.Instance, e.Closest, strings.Join(e.Missing, ", "))
}

// AmbiguousError is returned when an interface Want matches more than one
// registered concrete type under the same instance. Candidates preserves
// registration order.
type AmbiguousError struct {
	Want       reflect.Type
	Instance   string
	Candidates []reflect.Type
}

func (e *AmbiguousError) Error() string {
	names := make([]string, len(e.Candidates))
	for i, c := range e.Candidates {
		names[i] = c.String()
	}
	return fmt.Sprintf("xbc: type %s (instance %q) matched to %d candidates: %s",
		e.Want, e.Instance, len(e.Candidates), strings.Join(names, ", "))
}

// Provide registers v under the calling plugin's own instance name.
//
// T can be instantiated with an interface type, but reflect.TypeOf(v) always
// yields v's dynamic concrete type -- the registry only ever stores concrete
// types, matching the RuntimeHost implementation's assumption that its ordering
// enumerates concrete types.
func Provide[T any](ctx *Context, v T) {
	ctx.host.ProvideValue(reflect.TypeOf(v), ctx.Instance(), v)
}

// Get looks up the default instance of T. It is GetNamed with an empty name.
func Get[T any](ctx *Context) (T, bool) {
	return GetNamed[T](ctx, "")
}

// GetNamed looks up T under the given instance name ("" means default).
func GetNamed[T any](ctx *Context, name string) (T, bool) {
	var zero T
	want := typeOf[T]()
	v, err := ctx.host.LookupValue(want, NormalizeInstance(name))
	if err != nil {
		return zero, false
	}
	tv, ok := v.(T)
	if !ok {
		return zero, false
	}
	return tv, true
}

// MustGet is MustGetNamed with an empty name.
func MustGet[T any](ctx *Context) T {
	return MustGetNamed[T](ctx, "")
}

// MustGetNamed looks up T under the given instance name and panics with the
// full diagnostic (including any Closest/Missing/Candidates detail the RuntimeHost
// attached to the error) when it is not found or ambiguous.
func MustGetNamed[T any](ctx *Context, name string) T {
	want := typeOf[T]()
	inst := NormalizeInstance(name)
	v, err := ctx.host.LookupValue(want, inst)
	if err != nil {
		panic(err.Error())
	}
	tv, ok := v.(T)
	if !ok {
		panic(fmt.Sprintf("xbc: type assertion failed: value of %s (instance %q) in registry cannot be converted to %s", reflect.TypeOf(v), inst, want))
	}
	return tv
}
