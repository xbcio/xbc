// Package inject scans xbc struct tags off a plugin's fields via reflection.
//
// It knows nothing about xbc types -- it only understands reflect.Type,
// reflect.Value, and its own FieldSpec. This keeps the package independently
// testable and lets package assembly build the dependency graph on top of it
// without creating an import cycle.
package inject

import (
	"fmt"
	"reflect"
	"strings"
)

// Kind distinguishes the two ends of the dependency graph a tagged field
// describes: what a plugin wants (KindInject) versus what it produces
// (KindProvide). A single Scan call over one plugin's fields yields both
// ends at once, which is exactly why the graph can be built statically
// before any plugin's Init has run.
type Kind int

const (
	KindInject Kind = iota
	KindProvide
)

// FieldSpec describes one xbc-tagged field found on a plugin struct.
type FieldSpec struct {
	Index    int          // top-level field index; usable directly with reflect.Value.Field
	Name     string       // Go field name, for error copy
	Type     reflect.Type // field type, used for registry lookups and Set's type check
	Instance string       // "" means default; always "" for KindProvide
	Optional bool         // only meaningful for KindInject
	Kind     Kind
}

// Scan reads the xbc struct tags off a plugin value.
//
// Only exported top-level fields are considered; embedded structs (e.g. a
// plugin's embedded Base) are not recursed into -- an embedded field with no
// xbc tag of its own simply falls through the "no tag" branch below, just
// like any other untagged field.
//
// v must be a non-nil pointer to a struct.
func Scan(v any) ([]FieldSpec, error) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer {
		return nil, fmt.Errorf("inject: Scan needs a pointer to a struct, received %T", v)
	}
	if rv.IsNil() {
		return nil, fmt.Errorf("inject: Scan received nil pointer")
	}
	elem := rv.Elem()
	if elem.Kind() != reflect.Struct {
		return nil, fmt.Errorf("inject: Scan needs a pointer to a struct, received pointer to %s", elem.Kind())
	}

	t := elem.Type()
	var specs []FieldSpec
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)

		tagVal, ok := f.Tag.Lookup("xbc")
		if !ok {
			// No tag: not our business. This is exactly the branch an
			// embedded Base (or any other untagged field) falls into.
			continue
		}
		if !f.IsExported() {
			// Tag.Lookup works regardless of exportedness, but reflect can
			// never Set an unexported field. Skipping silently here would
			// mean the tag is a lie the author never finds out about until
			// something downstream sees an unexplained nil.
			return nil, fmt.Errorf("inject: field %s is unexported but has xbc tag, reflection cannot assign to it", f.Name)
		}
		if tagVal == "-" {
			continue
		}

		spec, err := parseTag(f, tagVal)
		if err != nil {
			return nil, err
		}
		spec.Index = i
		specs = append(specs, spec)
	}
	return specs, nil
}

// parseTag parses one field's xbc tag value, e.g. "inject,name=ro,optional".
func parseTag(f reflect.StructField, tag string) (FieldSpec, error) {
	parts := strings.Split(tag, ",")
	action := parts[0]

	spec := FieldSpec{Name: f.Name, Type: f.Type}
	switch action {
	case "inject":
		spec.Kind = KindInject
	case "provide":
		spec.Kind = KindProvide
	default:
		return FieldSpec{}, fmt.Errorf(
			"inject: field %s has unknown xbc tag action %q; valid values: inject, provide, -", f.Name, action)
	}

	for _, opt := range parts[1:] {
		key, val, hasVal := strings.Cut(opt, "=")
		switch key {
		case "name":
			if !hasVal {
				return FieldSpec{}, fmt.Errorf(
					"inject: field %s has no value for xbc tag option %q; expected name=<instance>", f.Name, opt)
			}
			if spec.Kind == KindProvide {
				return FieldSpec{}, fmt.Errorf(
					"inject: field %s uses provide and cannot specify name; provided values are scoped to the plugin's own instance", f.Name)
			}
			spec.Instance = val
		case "optional":
			if hasVal {
				return FieldSpec{}, fmt.Errorf(
					"inject: field %s has a value for xbc tag option %q, which accepts no value", f.Name, opt)
			}
			if spec.Kind == KindProvide {
				return FieldSpec{}, fmt.Errorf(
					"inject: field %s uses provide and cannot be optional", f.Name)
			}
			spec.Optional = true
		default:
			return FieldSpec{}, fmt.Errorf(
				"inject: field %s has unknown xbc tag option %q; valid values: name, optional", f.Name, key)
		}
	}
	return spec, nil
}

// Set assigns val to the field described by spec on v.
//
// The assignment is type-checked first via AssignableTo: an incompatible val
// reports an error instead of panicking, because a single misbehaving plugin
// panicking here would take down the whole assembly pipeline for everyone
// else riding in the same process.
func Set(v any, spec FieldSpec, val any) error {
	fv, err := fieldValue(v, spec)
	if err != nil {
		return err
	}
	if !fv.CanSet() {
		return fmt.Errorf("inject: field %s is not settable", spec.Name)
	}

	rv := reflect.ValueOf(val)
	if !rv.IsValid() {
		// val is an untyped nil; only legal for field types that can hold nil.
		switch spec.Type.Kind() {
		case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
			fv.Set(reflect.Zero(spec.Type))
			return nil
		default:
			return fmt.Errorf("inject: field %s type %s cannot be set to nil", spec.Name, spec.Type)
		}
	}
	if !rv.Type().AssignableTo(spec.Type) {
		return fmt.Errorf("inject: field %s type mismatch: expected %s, got %s", spec.Name, spec.Type, rv.Type())
	}
	fv.Set(rv)
	return nil
}

// IsZero reports whether the field described by spec is still its zero value.
func IsZero(v any, spec FieldSpec) (bool, error) {
	fv, err := fieldValue(v, spec)
	if err != nil {
		return false, err
	}
	return fv.IsZero(), nil
}

// Value returns the current value of the field described by spec.
func Value(v any, spec FieldSpec) (any, error) {
	fv, err := fieldValue(v, spec)
	if err != nil {
		return nil, err
	}
	return fv.Interface(), nil
}

// fieldValue resolves spec.Index against v. Set/IsZero/Value can be called
// independently of Scan (as the tests above do, building FieldSpec literals
// by hand), so this cannot assume v was ever passed to Scan -- it re-checks
// the same pointer-to-struct shape Scan enforces.
func fieldValue(v any, spec FieldSpec) (reflect.Value, error) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return reflect.Value{}, fmt.Errorf("inject: need a pointer to a struct, received %T", v)
	}
	elem := rv.Elem()
	if elem.Kind() != reflect.Struct {
		return reflect.Value{}, fmt.Errorf("inject: need a pointer to a struct, received a pointer to %s", elem.Kind())
	}
	if spec.Index < 0 || spec.Index >= elem.NumField() {
		return reflect.Value{}, fmt.Errorf("inject: field index %d out of range", spec.Index)
	}
	return elem.Field(spec.Index), nil
}
