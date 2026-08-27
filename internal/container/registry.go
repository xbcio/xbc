package container

import (
	"reflect"
	"sync"

	"github.com/xbcio/xbc/plugin"
)

// registryKey identifies one stored value by its concrete type and instance
// name. Shared between this file's valueRegistry and resolve.go's static
// product index -- both need the exact same (type, instance) identity
// concept, and resolve() reuses this type directly rather than defining its
// own local equivalent.
type registryKey struct {
	typ      reflect.Type
	instance string
}

// valueRegistry is the (type, instance) -> value store shared by every
// Context belonging to the same Container. It normalizes the instance
// argument itself at every entry point -- "" and "default" are the same key
// here, not just at the plugin.Provide/Get facade layer above it.
//
// Two Containers built from the same Snapshot each get their own
// valueRegistry (New allocates a fresh one every call), so nothing stored by
// one leaks into the other -- see TestNewContainersFromSameSnapshotAreIndependent
// in container_test.go for the pinned test of that property.
type valueRegistry struct {
	mu    sync.RWMutex
	m     map[registryKey]any
	order []registryKey // stable insertion order, for deterministic diagnostics
}

func newValueRegistry() *valueRegistry {
	return &valueRegistry{m: make(map[registryKey]any)}
}

// put stores v under (typ, instance). Re-putting the same key overwrites the
// value but does not change its position in order -- order only tracks the
// first registration of each key.
func (r *valueRegistry) put(typ reflect.Type, instance string, v any) {
	instance = plugin.NormalizeInstance(instance)

	r.mu.Lock()
	defer r.mu.Unlock()

	key := registryKey{typ: typ, instance: instance}
	if _, exists := r.m[key]; !exists {
		r.order = append(r.order, key)
	}
	r.m[key] = v
}

// lookup implements the three-branch registry semantics: exact hit for
// concrete types; for interface types, an exact hit first, then a scan of
// every concrete type registered under the same instance for
// AssignableTo(want). Both the NotFoundError and AmbiguousError returned
// here are plugin's own exported types (plugin/registry.go) -- reusing them
// rather than defining local duplicates is what lets plugin.Get[T]/
// GetNamed[T]/MustGetNamed[T] (which call through RuntimeHost.LookupValue, backed
// by this method) render the exact same diagnostic quality the old registry
// produced.
func (r *valueRegistry) lookup(want reflect.Type, instance string) (any, error) {
	instance = plugin.NormalizeInstance(instance)

	r.mu.RLock()
	defer r.mu.RUnlock()

	if v, ok := r.m[registryKey{typ: want, instance: instance}]; ok {
		return v, nil
	}

	if want.Kind() != reflect.Interface {
		return nil, &plugin.NotFoundError{Want: want, Instance: instance}
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
		return nil, &plugin.NotFoundError{Want: want, Instance: instance, Closest: closest, Missing: missing}
	case 1:
		return r.m[registryKey{typ: candidates[0], instance: instance}], nil
	default:
		return nil, &plugin.AmbiguousError{Want: want, Instance: instance, Candidates: candidates}
	}
}

// concreteTypes returns every concrete type registered under instance, in
// registration order.
func (r *valueRegistry) concreteTypes(instance string) []reflect.Type {
	instance = plugin.NormalizeInstance(instance)

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
