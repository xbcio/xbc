// extension.go
package plugin

import (
	"fmt"
	"reflect"
)

// Extension pairs a queried capability value with the Identity of the
// plugin instance that provided it, so a runtime stack can label a
// middleware, attribute a route, or report a diagnostic without reaching
// into the providing plugin's own Context.
type Extension[T any] struct {
	Identity Identity
	Value    T
}

// Extensions returns every enabled, already-initialized plugin instance
// that implements capability T, in core's dependency-topological order.
//
// Semantics are pinned (package-layout design §5.6):
//
//   - only enabled instances that have completed Init are considered;
//   - the result is a fresh slice on every call -- callers can freely
//     mutate what they get back without affecting a later call;
//   - T must be an interface type. Extensions queries a *capability*, not a
//     produced value -- that distinction is what separates it from
//     Get[T]/GetNamed[T], which look up a concrete provided value instead.
//     Passing a concrete type is therefore rejected rather than silently
//     returning zero matches;
//   - if the host has not finished initializing every plugin yet,
//     InitializedPlugins returns an error and Extensions propagates it
//     unchanged -- it never hands back a partial, still-initializing
//     snapshot dressed up as a complete one.
func Extensions[T any](ctx *Context) ([]Extension[T], error) {
	want := typeOf[T]()
	if want.Kind() != reflect.Interface {
		return nil, fmt.Errorf(
			"xbc: Extensions 查询的是插件能力（接口类型），不是具体产物值，得到 %s；查具体类型请使用 Get[T]", want)
	}

	all, err := ctx.host.InitializedPlugins()
	if err != nil {
		return nil, err
	}

	out := make([]Extension[T], 0, len(all))
	for _, e := range all {
		tv, ok := e.Value.(T)
		if !ok {
			continue
		}
		out = append(out, Extension[T]{Identity: e.Identity, Value: tv})
	}
	return out, nil
}
