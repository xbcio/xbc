package plugin

import "reflect"

// Dep describes either a typed value requirement (Deps.Types) or a product
// declaration (Provider.Provides). Instance and Optional are consumer-side
// fields; product declarations must leave both at their zero values because a
// product belongs to its producing plugin instance and cannot be optional.
type Dep struct {
	Type     reflect.Type
	Instance string // consumer target; "" means default
	Optional bool   // consumer requirement may be absent
}

// Ref is a hard dependency on another plugin's stable Key. An unspecified
// instance means the dependency applies to every enabled instance of that
// Definition; Instance narrows it to one named instance.
type Ref struct {
	key      Key
	instance string
}

// RefTo creates a hard plugin dependency by stable key.
func RefTo(key Key) Ref { return key.Ref() }

// Deps is a plugin's complete dependency and soft-ordering declaration.
type Deps struct {
	Types   []Dep
	Plugins []Ref
	After   []Key
	Before  []Key
}

func typeOf[T any]() reflect.Type { return reflect.TypeOf((*T)(nil)).Elem() }

func Need[T any]() Dep { return Dep{Type: typeOf[T]()} }
func NeedNamed[T any](instance string) Dep {
	return Dep{Type: typeOf[T](), Instance: instance}
}
func Opt[T any]() Dep   { return Dep{Type: typeOf[T](), Optional: true} }
func Offer[T any]() Dep { return Dep{Type: typeOf[T]()} }

// Instance narrows a Ref to one named instance.
func (r Ref) Instance(name string) Ref {
	r.instance = name
	return r
}

// Key returns the referenced plugin key.
func (r Ref) Key() Key { return r.key }

// InstanceName returns the requested instance, or "" for all enabled
// instances of the referenced Definition.
func (r Ref) InstanceName() string { return r.instance }

func (d Dep) String() string {
	s := "<nil>"
	if d.Type != nil {
		s = d.Type.String()
	}
	if inst := NormalizeInstance(d.Instance); inst != DefaultInstance {
		s += "[" + inst + "]"
	}
	return s
}

func (r Ref) String() string {
	s := r.key.String()
	if r.instance != "" {
		s += "[" + r.instance + "]"
	}
	return s
}
