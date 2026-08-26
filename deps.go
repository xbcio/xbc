package xbc

import "reflect"

// typeOf returns the reflect.Type for T itself, never for *T. Going through
// a *T pointer side-steps a specific trap: reflect.TypeOf(v) applied to a
// value of interface type T returns the *concrete* type stored inside the
// interface (or nil, for the zero value) -- never the interface type T
// itself. A *T value is never nil regardless of what T is, so TypeOf never
// has to guess, and .Elem() unwraps the pointer to hand back exactly T's own
// reflect.Type, whether T is an interface (io.Reader) or a concrete type
// (*bytes.Buffer).
func typeOf[T any]() reflect.Type {
	return reflect.TypeOf((*T)(nil)).Elem()
}

// normInstance normalizes an instance name for comparison and display: ""
// and "default" mean the same thing everywhere in xbc (the zero-value
// instance), so a caller that holds either spelling can normalize once
// instead of special-casing both at every comparison site.
func normInstance(s string) string {
	if s == "" {
		return defaultInstance
	}
	return s
}

// Need declares a hard dependency on the default instance of T. Stage 4's
// resolve() (stage_resolve.go) fails the whole app if no plugin provides one.
func Need[T any]() Dep {
	return Dep{Type: typeOf[T]()}
}

// NeedNamed declares a hard dependency on a specific, non-default instance
// of T, e.g. NeedNamed[*gorm.DB]("readonly").
func NeedNamed[T any](instance string) Dep {
	return Dep{Type: typeOf[T](), Instance: instance}
}

// Opt declares a soft dependency: if the default instance of T exists it is
// wired up, but its absence is not an error.
func Opt[T any]() Dep {
	return Dep{Type: typeOf[T](), Optional: true}
}

// Offer declares that a plugin's Provider.Provides() produces an instance of
// T. It is spelled distinctly from Need -- even though the two build an
// identical Dep{Type: typeOf[T]()} value -- because the two lists that carry
// them (Deps.Types vs Provider's return value) are read in opposite
// directions: one says "resolve me one of these", the other says "I am the
// one you can resolve to". Sharing the constructor name would make code that
// mixes them up (declaring a need in Provides, say) read as correct when it
// isn't.
func Offer[T any]() Dep {
	return Dep{Type: typeOf[T]()}
}

// RefOf declares a hard dependency on another plugin, by identity rather
// than by any type it provides.
//
// T is constrained to Plugin, not any, because a Ref names one specific
// plugin by its own concrete type -- unlike Dep, which is routinely an
// interface (Need[io.Reader]() correctly means "whatever provides an
// io.Reader"), there is no such thing as "any plugin that happens to
// implement io.Reader" for a Ref to mean. RefOf[T any]() would let
// RefOf[string]() compile and produce a Ref that can never correspond to a
// real registration; RefOf[T Plugin]() turns that into a compile error
// instead, at no cost to any real call site, which always passes a concrete
// *SomePlugin type anyway.
func RefOf[T Plugin]() Ref {
	return Ref{typ: typeOf[T]()}
}

// Instance narrows a Ref to one specific instance of the referenced plugin.
// The zero value ("") means any instance of that plugin will do.
func (r Ref) Instance(name string) Ref {
	r.instance = name
	return r
}

// String renders a Dep for error copy, e.g. "*gorm.DB" or
// "*gorm.DB[readonly]". The bracket is omitted for the default instance --
// normInstance folds both "" and the explicit string "default" to the same
// unqualified rendering, so a caller who wrote NeedNamed[T]("default")
// instead of Need[T]() doesn't get a redundant "[default]" in every error
// message.
func (d Dep) String() string {
	s := d.Type.String()
	if inst := normInstance(d.Instance); inst != defaultInstance {
		s += "[" + inst + "]"
	}
	return s
}

// String renders a Ref the same way Dep does, e.g. "*jwt.Plugin[readonly]".
//
// Unlike Dep.String, this does NOT run r.instance through normInstance: a
// Ref's "" means "any instance is acceptable", which is a different thing
// from Dep's "" ("the default instance specifically") and must not be
// displayed as if the caller had asked for "default" by name.
func (r Ref) String() string {
	s := r.typ.String()
	if r.instance != "" {
		s += "[" + r.instance + "]"
	}
	return s
}
