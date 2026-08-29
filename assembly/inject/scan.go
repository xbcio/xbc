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
		return nil, fmt.Errorf("inject: Scan 需要指向结构体的指针，收到 %T", v)
	}
	if rv.IsNil() {
		return nil, fmt.Errorf("inject: Scan 收到 nil 指针")
	}
	elem := rv.Elem()
	if elem.Kind() != reflect.Struct {
		return nil, fmt.Errorf("inject: Scan 需要指向结构体的指针，收到指向 %s 的指针", elem.Kind())
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
			return nil, fmt.Errorf("inject: 字段 %s 未导出但带有 xbc tag，反射无法为它赋值", f.Name)
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
			"inject: 字段 %s 的 xbc tag 动作 %q 未知，合法取值：inject、provide、-", f.Name, action)
	}

	for _, opt := range parts[1:] {
		key, val, hasVal := strings.Cut(opt, "=")
		switch key {
		case "name":
			if !hasVal {
				return FieldSpec{}, fmt.Errorf(
					"inject: 字段 %s 的 xbc tag 选项 %q 缺少值，期望 name=<实例名>", f.Name, opt)
			}
			if spec.Kind == KindProvide {
				return FieldSpec{}, fmt.Errorf(
					"inject: 字段 %s 是 provide，不能指定 name —— 产物实例名来自插件自己，不能由 tag 指定", f.Name)
			}
			spec.Instance = val
		case "optional":
			if hasVal {
				return FieldSpec{}, fmt.Errorf(
					"inject: 字段 %s 的 xbc tag 选项 %q 不接受值", f.Name, opt)
			}
			if spec.Kind == KindProvide {
				return FieldSpec{}, fmt.Errorf(
					"inject: 字段 %s 是 provide，产物没有可选一说，optional 无意义", f.Name)
			}
			spec.Optional = true
		default:
			return FieldSpec{}, fmt.Errorf(
				"inject: 字段 %s 的 xbc tag 选项 %q 未知，合法取值：name、optional", f.Name, key)
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
		return fmt.Errorf("inject: 字段 %s 不可设置", spec.Name)
	}

	rv := reflect.ValueOf(val)
	if !rv.IsValid() {
		// val is an untyped nil; only legal for field types that can hold nil.
		switch spec.Type.Kind() {
		case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
			fv.Set(reflect.Zero(spec.Type))
			return nil
		default:
			return fmt.Errorf("inject: 字段 %s 类型 %s 不能被赋值为 nil", spec.Name, spec.Type)
		}
	}
	if !rv.Type().AssignableTo(spec.Type) {
		return fmt.Errorf("inject: 字段 %s 类型不匹配：期望 %s，得到 %s", spec.Name, spec.Type, rv.Type())
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
		return reflect.Value{}, fmt.Errorf("inject: 需要指向结构体的指针，收到 %T", v)
	}
	elem := rv.Elem()
	if elem.Kind() != reflect.Struct {
		return reflect.Value{}, fmt.Errorf("inject: 需要指向结构体的指针，收到指向 %s 的指针", elem.Kind())
	}
	if spec.Index < 0 || spec.Index >= elem.NumField() {
		return reflect.Value{}, fmt.Errorf("inject: 字段索引 %d 超出范围", spec.Index)
	}
	return elem.Field(spec.Index), nil
}
