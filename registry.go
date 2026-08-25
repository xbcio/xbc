// registry.go
package xbc

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
)

// registryKey identifies one stored value by its concrete type and instance name.
type registryKey struct {
	typ      reflect.Type
	instance string
}

// registry is the (type, instance) -> value store shared by every Context
// that belongs to the same App. It never normalizes the instance argument --
// that is the caller's job (see normInstance in deps.go), so this type can
// be tested in isolation from the "" == "default" convention.
type registry struct {
	mu    sync.RWMutex
	m     map[registryKey]any
	order []registryKey // stable insertion order, for deterministic diagnostics
}

func newRegistry() *registry {
	return &registry{m: make(map[registryKey]any)}
}

// put stores v under (typ, instance). Re-putting the same key overwrites the
// value but does not change its position in order -- order only tracks the
// first registration of each key.
func (r *registry) put(typ reflect.Type, instance string, v any) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := registryKey{typ: typ, instance: instance}
	if _, exists := r.m[key]; !exists {
		r.order = append(r.order, key)
	}
	r.m[key] = v
}

// lookup implements the three-branch semantics documented in the API
// contract §5.6: exact hit for concrete types; for interface types, an
// exact hit first, then a scan of every concrete type registered under the
// same instance for AssignableTo(want).
func (r *registry) lookup(want reflect.Type, instance string) (any, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if v, ok := r.m[registryKey{typ: want, instance: instance}]; ok {
		return v, nil
	}

	if want.Kind() != reflect.Interface {
		return nil, &NotFoundError{Want: want, Instance: instance}
	}

	var candidates []reflect.Type
	for _, key := range r.order {
		if key.instance != instance {
			continue
		}
		if key.typ.AssignableTo(want) {
			candidates = append(candidates, key.typ)
		}
	}

	switch len(candidates) {
	case 0:
		closest, missing := closestMatch(r.order, instance, want)
		return nil, &NotFoundError{Want: want, Instance: instance, Closest: closest, Missing: missing}
	case 1:
		return r.m[registryKey{typ: candidates[0], instance: instance}], nil
	default:
		return nil, &AmbiguousError{Want: want, Instance: instance, Candidates: candidates}
	}
}

// concreteTypes returns every concrete type registered under instance, in
// registration order. Used by later stages (e.g. startup diagnostics) that
// need to enumerate what an instance actually provides.
func (r *registry) concreteTypes(instance string) []reflect.Type {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []reflect.Type
	for _, key := range r.order {
		if key.instance == instance {
			out = append(out, key.typ)
		}
	}
	return out
}

// closestMatch picks, among every concrete type registered under instance,
// the one implementing the most methods of want by name (signature
// mismatches still count as "missing"). Ties keep the earliest registered
// type, which is why the scan walks order rather than the map.
func closestMatch(order []registryKey, instance string, want reflect.Type) (reflect.Type, []string) {
	var best reflect.Type
	var bestMissing []string
	bestScore := -1

	for _, key := range order {
		if key.instance != instance {
			continue
		}
		score, missing := methodScore(key.typ, want)
		if score > bestScore {
			bestScore = score
			best = key.typ
			bestMissing = missing
		}
	}
	return best, bestMissing
}

// methodScore counts how many of want's methods candidate also has, by name only.
func methodScore(candidate, want reflect.Type) (score int, missing []string) {
	for i := 0; i < want.NumMethod(); i++ {
		name := want.Method(i).Name
		if _, ok := candidate.MethodByName(name); ok {
			score++
		} else {
			missing = append(missing, name)
		}
	}
	return score, missing
}

// NotFoundError is returned when lookup finds nothing for (Want, Instance).
// Closest is nil for a concrete Want (there is no "closest" concept when the
// match is exact-or-nothing) and also nil when the instance has nothing
// registered at all.
//
// Error renders the reusable core message only -- it does not know which
// plugin was asking, so it does not carry a plugin name. The stage that
// resolves dependencies (stage_resolve.go) wraps this with the plugin name
// to produce the exact copy in the API contract §13.
type NotFoundError struct {
	Want     reflect.Type
	Instance string
	Closest  reflect.Type
	Missing  []string
}

func (e *NotFoundError) Error() string {
	if e.Closest == nil {
		return fmt.Sprintf("xbc: 未找到类型 %s（实例 %q），该实例下未登记任何类型", e.Want, e.Instance)
	}
	return fmt.Sprintf("xbc: 未找到类型 %s（实例 %q），最接近的是 %s，缺少方法：%s",
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
	return fmt.Sprintf("xbc: 类型 %s（实例 %q）匹配到 %d 个候选：%s",
		e.Want, e.Instance, len(e.Candidates), strings.Join(names, ", "))
}

// Provide registers v under the calling plugin's own instance name.
//
// T can be instantiated with an interface type, but reflect.TypeOf(v) always
// yields v's dynamic concrete type -- the registry only ever stores concrete
// types, matching lookup's assumption that r.order enumerates concrete types.
func Provide[T any](ctx *Context, v T) {
	ctx.registry().put(reflect.TypeOf(v), ctx.Instance(), v)
}

// Get looks up the default instance of T. It is GetNamed with an empty name.
func Get[T any](ctx *Context) (T, bool) {
	return GetNamed[T](ctx, "")
}

// GetNamed looks up T under the given instance name ("" means default).
func GetNamed[T any](ctx *Context, name string) (T, bool) {
	var zero T
	want := typeOf[T]()
	v, err := ctx.registry().lookup(want, normInstance(name))
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
// full diagnostic (including any Closest/Missing/Candidates detail) when it
// is not found or ambiguous.
func MustGetNamed[T any](ctx *Context, name string) T {
	want := typeOf[T]()
	inst := normInstance(name)
	v, err := ctx.registry().lookup(want, inst)
	if err != nil {
		panic(err.Error())
	}
	tv, ok := v.(T)
	if !ok {
		panic(fmt.Sprintf("xbc: 类型断言失败：注册表中 %s（实例 %q）的值无法转换为 %s", reflect.TypeOf(v), inst, want))
	}
	return tv
}
