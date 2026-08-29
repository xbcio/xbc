package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

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

func joinIndexedPath(prefix, child string) string {
	if prefix == "" {
		return child
	}
	return prefix + "." + child
}
