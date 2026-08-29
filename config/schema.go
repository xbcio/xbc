package config

import (
	"encoding"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// leaf is one bindable leaf in a struct's yaml-tag schema. Path is relative
// until rootedLeaves attaches the section passed to Bind.
type leaf struct {
	Path       string
	Index      []int
	Type       reflect.Type
	Default    string
	HasDefault bool
}

// schemaNode is the strict-decoding shape for one value. Struct fields are
// closed; map keys are dynamic while MapElem remains recursively checked;
// Elem carries the schema for a slice/array element.
type schemaNode struct {
	Fields  map[string]*schemaNode
	Elem    *schemaNode
	MapElem *schemaNode
	Open    bool
}

type configSchema struct {
	Root         reflect.Type
	Node         *schemaNode
	Leaves       []leaf
	YAMLByGoPath map[string]string
}

func inspectStructPointer(out any) (reflect.Value, error) {
	if out == nil {
		return reflect.Value{}, fmt.Errorf("xbc: configuration target must be a non-nil struct pointer, got <nil>")
	}

	v := reflect.ValueOf(out)
	if v.Kind() != reflect.Pointer {
		return reflect.Value{}, fmt.Errorf("xbc: configuration target must be a non-nil struct pointer, got %T", out)
	}
	if v.IsNil() {
		return reflect.Value{}, fmt.Errorf("xbc: configuration target must be a non-nil struct pointer, got %s(nil)", v.Type())
	}
	if v.Elem().Kind() != reflect.Struct {
		return reflect.Value{}, fmt.Errorf("xbc: configuration target must be a non-nil struct pointer, got %s", v.Type())
	}
	return v.Elem(), nil
}

func schemaFor(out any) (reflect.Value, *configSchema, error) {
	root, err := inspectStructPointer(out)
	if err != nil {
		return reflect.Value{}, nil, err
	}
	schema, err := schemaForType(root.Type())
	if err != nil {
		return reflect.Value{}, nil, err
	}
	return root, schema, nil
}

func schemaForType(root reflect.Type) (*configSchema, error) {
	for root.Kind() == reflect.Pointer {
		root = root.Elem()
	}
	if root.Kind() != reflect.Struct {
		return nil, fmt.Errorf("xbc: configuration schema must be a struct, got %s", root)
	}

	schema := &configSchema{
		Root:         root,
		YAMLByGoPath: make(map[string]string),
	}
	node, err := schema.walkStruct(root, "", nil, nil, make(map[reflect.Type]bool), true)
	if err != nil {
		return nil, fmt.Errorf("xbc: configuration schema %s is invalid: %w", root, err)
	}
	schema.Node = node
	return schema, nil
}

func (s *configSchema) walkStruct(
	t reflect.Type,
	yamlPrefix string,
	indexPrefix []int,
	goPrefix []string,
	stack map[reflect.Type]bool,
	collectLeaves bool,
) (*schemaNode, error) {
	if stack[t] {
		return nil, fmt.Errorf("recursive struct %s is not supported", t)
	}
	stack[t] = true
	defer delete(stack, t)

	node := &schemaNode{Fields: make(map[string]*schemaNode)}
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue
		}

		name, inline, skip := yamlField(field)
		if skip {
			continue
		}

		fieldType := dereference(field.Type)
		if inline {
			if !field.Anonymous || !isStructSchema(fieldType) {
				return nil, fmt.Errorf("yaml inline for field %s only supports anonymous struct or *struct", field.Name)
			}
			if name != "" {
				return nil, fmt.Errorf("yaml inline for field %s cannot simultaneously declare name %q", field.Name, name)
			}
		}

		fieldIndex := appendIndex(indexPrefix, i)
		goPath := appendString(goPrefix, field.Name)
		yamlPath := yamlPrefix
		if !inline {
			yamlPath = joinPath(yamlPrefix, name)
		}
		s.YAMLByGoPath[strings.Join(goPath, ".")] = yamlPath

		if isStructSchema(fieldType) {
			child, err := s.walkStruct(fieldType, yamlPath, fieldIndex, goPath, stack, collectLeaves)
			if err != nil {
				return nil, err
			}
			if inline {
				if err := mergeSchemaFields(node.Fields, child.Fields, yamlPrefix); err != nil {
					return nil, err
				}
				continue
			}
			if _, exists := node.Fields[name]; exists {
				return nil, duplicateYAMLField(yamlPath)
			}
			node.Fields[name] = child
			continue
		}

		child, err := s.walkValue(field.Type, yamlPath, goPath, stack)
		if err != nil {
			return nil, err
		}
		if _, exists := node.Fields[name]; exists {
			return nil, duplicateYAMLField(yamlPath)
		}
		node.Fields[name] = child

		if collectLeaves {
			def, hasDefault := field.Tag.Lookup("default")
			s.Leaves = append(s.Leaves, leaf{
				Path:       yamlPath,
				Index:      fieldIndex,
				Type:       field.Type,
				Default:    def,
				HasDefault: hasDefault,
			})
		}
	}
	return node, nil
}

// walkValue preserves strictness through arbitrarily nested collection
// shapes (for example [][]Item or map[string][]Item). Interface values are
// the only explicit free-form boundary.
func (s *configSchema) walkValue(
	t reflect.Type,
	yamlPath string,
	goPath []string,
	stack map[reflect.Type]bool,
) (*schemaNode, error) {
	t = dereference(t)
	if isStructSchema(t) {
		return s.walkStruct(t, yamlPath, nil, goPath, stack, false)
	}

	node := &schemaNode{}
	switch t.Kind() {
	case reflect.Interface:
		node.Open = true
	case reflect.Map:
		child, err := s.walkValue(t.Elem(), yamlPath, goPath, stack)
		if err != nil {
			return nil, err
		}
		node.MapElem = child
	case reflect.Slice, reflect.Array:
		child, err := s.walkValue(t.Elem(), yamlPath, goPath, stack)
		if err != nil {
			return nil, err
		}
		node.Elem = child
	}
	return node, nil
}

func yamlField(field reflect.StructField) (name string, inline, skip bool) {
	tag, tagged := field.Tag.Lookup("yaml")
	if !tagged || tag == "" {
		return field.Name, false, false
	}

	parts := strings.Split(tag, ",")
	if parts[0] == "-" {
		return "", false, true
	}
	name = parts[0]
	for _, option := range parts[1:] {
		if option == "inline" {
			inline = true
		}
	}
	if name == "" && !inline {
		name = field.Name
	}
	return name, inline, false
}

var textUnmarshalerType = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()

func isStructSchema(t reflect.Type) bool {
	if t.Kind() != reflect.Struct || t == reflect.TypeOf(time.Time{}) {
		return false
	}
	return !t.Implements(textUnmarshalerType) && !reflect.PointerTo(t).Implements(textUnmarshalerType)
}

func dereference(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func mergeSchemaFields(dst, src map[string]*schemaNode, prefix string) error {
	keys := make([]string, 0, len(src))
	for key := range src {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, exists := dst[key]; exists {
			return duplicateYAMLField(joinPath(prefix, key))
		}
		dst[key] = src[key]
	}
	return nil
}

func duplicateYAMLField(path string) error {
	return fmt.Errorf("yaml path %s is declared by multiple fields", displayPath(path))
}

func appendIndex(prefix []int, index int) []int {
	out := make([]int, len(prefix)+1)
	copy(out, prefix)
	out[len(prefix)] = index
	return out
}

func appendString(prefix []string, value string) []string {
	out := make([]string, len(prefix)+1)
	copy(out, prefix)
	out[len(prefix)] = value
	return out
}

func (s *configSchema) rootedLeaves(root string) []leaf {
	out := make([]leaf, len(s.Leaves))
	for i, item := range s.Leaves {
		out[i] = item
		out[i].Path = joinPath(root, item.Path)
	}
	return out
}

// leaves remains a small package-private convenience for tests and sync code;
// all schema consumers are backed by configSchema's single walk.
func leaves(root string, out any) []leaf {
	_, schema, err := schemaFor(out)
	if err != nil {
		return nil
	}
	return schema.rootedLeaves(root)
}

func fieldByIndex(root reflect.Value, index []int, allocate bool) (reflect.Value, bool) {
	value := root
	for depth, fieldIndex := range index {
		for value.Kind() == reflect.Pointer {
			if value.IsNil() {
				if !allocate || !value.CanSet() {
					return reflect.Value{}, false
				}
				value.Set(reflect.New(value.Type().Elem()))
			}
			value = value.Elem()
		}
		if value.Kind() != reflect.Struct {
			return reflect.Value{}, false
		}
		value = value.Field(fieldIndex)
		if depth < len(index)-1 && value.Kind() == reflect.Pointer && value.IsNil() && !allocate {
			return reflect.Value{}, false
		}
	}
	return value, true
}

func joinPath(path, sub string) string {
	switch {
	case path == "":
		return sub
	case sub == "":
		return path
	default:
		return path + "." + sub
	}
}
