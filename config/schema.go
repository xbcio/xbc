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
		return reflect.Value{}, fmt.Errorf("xbc: 配置目标必须是非 nil 的 struct 指针，得到 <nil>")
	}

	v := reflect.ValueOf(out)
	if v.Kind() != reflect.Pointer {
		return reflect.Value{}, fmt.Errorf("xbc: 配置目标必须是非 nil 的 struct 指针，得到 %T", out)
	}
	if v.IsNil() {
		return reflect.Value{}, fmt.Errorf("xbc: 配置目标必须是非 nil 的 struct 指针，得到 %s(nil)", v.Type())
	}
	if v.Elem().Kind() != reflect.Struct {
		return reflect.Value{}, fmt.Errorf("xbc: 配置目标必须是非 nil 的 struct 指针，得到 %s", v.Type())
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
		return nil, fmt.Errorf("xbc: 配置 schema 必须是 struct，得到 %s", root)
	}

	schema := &configSchema{
		Root:         root,
		YAMLByGoPath: make(map[string]string),
	}
	node, err := schema.walkStruct(root, "", nil, nil, make(map[reflect.Type]bool), true)
	if err != nil {
		return nil, fmt.Errorf("xbc: 配置 schema %s 无效：%w", root, err)
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
		return nil, fmt.Errorf("递归 struct %s 不受支持", t)
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
				return nil, fmt.Errorf("字段 %s 的 yaml:\",inline\" 仅支持匿名 struct 或 *struct", field.Name)
			}
			if name != "" {
				return nil, fmt.Errorf("字段 %s 的 yaml inline 不能同时声明名称 %q", field.Name, name)
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
	return fmt.Errorf("yaml 路径 %s 被多个字段声明", displayPath(path))
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

func (s *configSchema) unknownFields(value any, root string, allowed map[string]struct{}) []string {
	var unknown []string
	walkUnknown(s.Node, reflect.ValueOf(value), "", root, allowed, &unknown)
	sort.Strings(unknown)
	return unknown
}

func walkUnknown(
	node *schemaNode,
	value reflect.Value,
	relativePath string,
	root string,
	allowed map[string]struct{},
	unknown *[]string,
) {
	if node == nil || node.Open {
		return
	}
	for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
		if value.IsNil() {
			return
		}
		value = value.Elem()
	}
	if !value.IsValid() {
		return
	}

	if value.Kind() == reflect.Slice || value.Kind() == reflect.Array {
		if node.Elem == nil {
			return
		}
		for i := 0; i < value.Len(); i++ {
			indexed := fmt.Sprintf("%s[%d]", relativePath, i)
			walkUnknown(node.Elem, value.Index(i), indexed, root, allowed, unknown)
		}
		return
	}
	if value.Kind() == reflect.Map && node.MapElem != nil {
		keys := value.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i].Interface()) < fmt.Sprint(keys[j].Interface()) })
		for _, key := range keys {
			dynamicPath := joinIndexedPath(relativePath, fmt.Sprint(key.Interface()))
			walkUnknown(node.MapElem, value.MapIndex(key), dynamicPath, root, allowed, unknown)
		}
		return
	}
	if value.Kind() != reflect.Map || value.Type().Key().Kind() != reflect.String {
		return
	}

	keys := value.MapKeys()
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	for _, keyValue := range keys {
		key := keyValue.String()
		childPath := joinIndexedPath(relativePath, key)
		child, ok := node.Fields[key]
		if ok {
			// A real schema field always wins over the framework allowlist. An
			// accidental overlap must not disable strict checking below it.
			walkUnknown(child, value.MapIndex(keyValue), childPath, root, allowed, unknown)
			continue
		}
		if _, ok := allowed[childPath]; ok {
			continue
		}
		if hasAllowedDescendant(allowed, childPath) {
			walkAllowedOnly(value.MapIndex(keyValue), childPath, root, allowed, unknown)
		} else {
			*unknown = append(*unknown, joinPath(root, childPath))
		}
	}
}

func walkAllowedOnly(value reflect.Value, relativePath, root string, allowed map[string]struct{}, unknown *[]string) {
	for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
		if value.IsNil() {
			return
		}
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Map || value.Type().Key().Kind() != reflect.String {
		*unknown = append(*unknown, joinPath(root, relativePath))
		return
	}

	keys := value.MapKeys()
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	for _, keyValue := range keys {
		childPath := joinIndexedPath(relativePath, keyValue.String())
		if _, ok := allowed[childPath]; ok {
			continue
		}
		if hasAllowedDescendant(allowed, childPath) {
			walkAllowedOnly(value.MapIndex(keyValue), childPath, root, allowed, unknown)
			continue
		}
		*unknown = append(*unknown, joinPath(root, childPath))
	}
}

func hasAllowedDescendant(allowed map[string]struct{}, path string) bool {
	prefix := path + "."
	for candidate := range allowed {
		if strings.HasPrefix(candidate, prefix) {
			return true
		}
	}
	return false
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

func joinIndexedPath(prefix, child string) string {
	if prefix == "" {
		return child
	}
	return prefix + "." + child
}

func prepareInlinePointers(root reflect.Value, rootType reflect.Type, input any) []reflect.Value {
	inputValue, ok := stringMapValue(reflect.ValueOf(input))
	if !ok {
		return nil
	}
	var temporary []reflect.Value
	prepareStructPointers(root, rootType, inputValue, &temporary)
	return temporary
}

func prepareStructPointers(root reflect.Value, rootType reflect.Type, input reflect.Value, temporary *[]reflect.Value) {
	for i := 0; i < rootType.NumField(); i++ {
		field := rootType.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name, inline, skip := yamlField(field)
		fieldType := dereference(field.Type)
		if skip || !isStructSchema(fieldType) {
			continue
		}

		fieldValue := root.Field(i)
		childInput := input
		matched := true
		if inline {
			matched = mapContainsStructField(input, fieldType)
		} else {
			var found bool
			childInput, found = mapField(input, name)
			if !found {
				continue
			}
			if _, ok := stringMapValue(childInput); !ok {
				continue
			}
		}

		if fieldValue.Kind() == reflect.Pointer {
			if fieldValue.IsNil() {
				fieldValue.Set(reflect.New(fieldValue.Type().Elem()))
				if inline && !matched {
					*temporary = append(*temporary, fieldValue)
				}
			}
			fieldValue = fieldValue.Elem()
		}
		childInput, ok := stringMapValue(childInput)
		if !ok {
			continue
		}
		prepareStructPointers(fieldValue, fieldType, childInput, temporary)
	}
}

func stringMapValue(value reflect.Value) (reflect.Value, bool) {
	for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
		if value.IsNil() {
			return reflect.Value{}, false
		}
		value = value.Elem()
	}
	return value, value.IsValid() && value.Kind() == reflect.Map && value.Type().Key().Kind() == reflect.String
}

func mapField(input reflect.Value, name string) (reflect.Value, bool) {
	for _, key := range input.MapKeys() {
		if key.String() == name {
			return input.MapIndex(key), true
		}
	}
	return reflect.Value{}, false
}

func mapContainsStructField(input reflect.Value, structType reflect.Type) bool {
	for i := 0; i < structType.NumField(); i++ {
		field := structType.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name, inline, skip := yamlField(field)
		if skip {
			continue
		}
		if inline {
			if mapContainsStructField(input, dereference(field.Type)) {
				return true
			}
			continue
		}
		for _, key := range input.MapKeys() {
			if key.String() == name {
				return true
			}
		}
	}
	return false
}
