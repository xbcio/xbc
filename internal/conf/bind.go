// internal/conf/bind.go
package conf

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/knadh/koanf/v2"
)

// Leaf is one scalar leaf in a struct's yaml-tag schema.
type Leaf struct {
	Path  string // e.g. "plugins.gorm.default.max_open_conn"
	Index []int  // reflect field index path from the struct root
	Type  reflect.Type
}

// Leaves walks out's yaml-tag schema and returns every leaf, with Path
// rooted at root (root == "" means unrooted, paths start at the struct itself).
//
// Two current limitations of this walk, both by design-gap rather than bug,
// and both worth knowing before writing a plugin Config:
//
//  1. Pointer-typed sub-struct fields (e.g. `Pool *PoolConfig`) are NOT
//     recursed into -- walkLeaves treats them as an opaque scalar leaf (see
//     walkLeaves) and setScalar has no case for reflect.Ptr, so if the ENV
//     or default machinery ever reaches such a leaf it fails with "不支持的
//     标量类型" rather than descending into it. In practice this means: a
//     value written for that subtree in the config file still binds fine
//     (mapstructure allocates and populates the pointer directly), but ENV
//     overrides and `default` tags on leaves *inside* that pointed-to struct
//     are silently unreachable -- Bind will not error, it will simply never
//     look for XBC_..._POOL_... variables or apply their default tags. This
//     is deliberate: eagerly allocating a nil pointer during a schema walk
//     would materialize a config subtree the user never wrote, and deciding
//     when to lazily allocate one (e.g. "only if some ENV var under it is
//     set") is a separate design problem left for a future task.
//  2. Exported anonymous embedded structs ARE recursed into, but walkLeaves
//     never special-cases f.Anonymous, so the embedded field's Go type name
//     (e.g. "PoolConfig" for `PoolConfig` embedded under Server) is always
//     appended as its own path segment -- even a `yaml:",inline"` tag on the
//     embedded field is not honored, because yamlTagName falls back to
//     f.Name whenever the tag's name component is empty. The caller ends up
//     with a path like "server.PoolConfig.field" instead of the flattened
//     "server.field" that struct embedding usually implies for YAML users.
func Leaves(root string, out any) []Leaf {
	t := reflect.TypeOf(out)
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	var leaves []Leaf
	walkLeaves(root, t, nil, &leaves)
	return leaves
}

// walkLeaves recurses only into plain (non-pointer) structs -- time.Time is
// treated as a scalar (it has no yaml-tagged fields of its own that this
// scheme cares about), and reflect.Ptr struct fields are treated as opaque
// scalar leaves rather than recursed into. See the two numbered limitations
// on Leaves' doc comment for what that means for pointer sub-structs and for
// anonymous embedded structs.
func walkLeaves(prefix string, t reflect.Type, index []int, out *[]Leaf) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported field: mapstructure/yaml can't reach it either
		}
		name := yamlTagName(f)
		if name == "-" {
			continue
		}

		path := prefix
		if name != "" {
			if path != "" {
				path += "." + name
			} else {
				path = name
			}
		}

		fieldIndex := append(append([]int{}, index...), i)

		ft := f.Type
		if ft.Kind() == reflect.Struct && ft != reflect.TypeOf(time.Time{}) {
			walkLeaves(path, ft, fieldIndex, out)
			continue
		}
		*out = append(*out, Leaf{Path: path, Index: fieldIndex, Type: ft})
	}
}

func yamlTagName(f reflect.StructField) string {
	tag := f.Tag.Get("yaml")
	if tag == "" {
		return f.Name
	}
	name := strings.Split(tag, ",")[0]
	if name == "" {
		return f.Name
	}
	return name
}

// EnvName maps a config path to its environment variable name:
// "plugins.gorm.default.dsn" -> "XBC_PLUGINS_GORM_DEFAULT_DSN".
func EnvName(prefix, path string) string {
	return prefix + strings.ToUpper(strings.ReplaceAll(path, ".", "_"))
}

// Bind performs the stage-3 chain for one subtree:
//  1. unmarshal k's subtree at path into out (mapstructure, Tag "yaml")
//  2. overlay ENV, driven by out's own yaml-tag schema (ruling R2)
//  3. fill `default:"..."` tags on leaves that neither the file nor ENV set
//
// It does not validate.
func Bind(k *koanf.Koanf, path string, out any, envPrefix string) error {
	if err := k.UnmarshalWithConf(path, out, koanf.UnmarshalConf{Tag: "yaml"}); err != nil {
		return fmt.Errorf("xbc: 绑定配置节 %s 失败：%w", displayPath(path), err)
	}

	leaves := Leaves(path, out)
	v := reflect.ValueOf(out).Elem()

	envSet := make(map[string]bool, len(leaves))
	for _, leaf := range leaves {
		envName := EnvName(envPrefix, leaf.Path)
		s, ok := os.LookupEnv(envName)
		if !ok {
			continue
		}
		envSet[leaf.Path] = true
		fv := fieldByIndex(v, leaf.Index)
		if err := setScalar(fv, leaf.Type, s); err != nil {
			return fmt.Errorf("xbc: 环境变量 %s 的值 %q 无法解析为 %s：%w", envName, s, leaf.Type, err)
		}
	}

	for _, leaf := range leaves {
		if envSet[leaf.Path] || k.Exists(leaf.Path) {
			continue
		}
		defTag, ok := defaultTag(reflect.TypeOf(out), leaf.Index)
		if !ok {
			continue
		}
		fv := fieldByIndex(v, leaf.Index)
		if err := setScalar(fv, leaf.Type, defTag); err != nil {
			return fmt.Errorf("xbc: 字段 %s 的 default tag %q 无法解析为 %s：%w", leaf.Path, defTag, leaf.Type, err)
		}
	}

	return nil
}

func displayPath(path string) string {
	if path == "" {
		return "(root)"
	}
	return path
}

func fieldByIndex(v reflect.Value, index []int) reflect.Value {
	for _, i := range index {
		v = v.Field(i)
	}
	return v
}

// defaultTag reads the `default:"..."` tag off the field at the end of index,
// walking through intermediate struct types along the way.
func defaultTag(t reflect.Type, index []int) (string, bool) {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	for depth, i := range index {
		f := t.Field(i)
		if depth == len(index)-1 {
			tag := f.Tag.Get("default")
			return tag, tag != ""
		}
		t = f.Type
		for t.Kind() == reflect.Ptr {
			t = t.Elem()
		}
	}
	return "", false
}

// setScalar parses s according to typ and stores it into v.
//
// time.Duration must be checked before the generic int64 case -- it is
// itself backed by int64, and checking order the other way around would try
// to parse "1h" as a base-10 integer and fail.
func setScalar(v reflect.Value, typ reflect.Type, s string) error {
	switch {
	case typ == reflect.TypeOf(time.Duration(0)):
		d, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		v.SetInt(int64(d))
		return nil
	case typ.Kind() == reflect.String:
		v.SetString(s)
		return nil
	case typ.Kind() == reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		v.SetBool(b)
		return nil
	case typ.Kind() >= reflect.Int && typ.Kind() <= reflect.Int64:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return err
		}
		v.SetInt(n)
		return nil
	case typ.Kind() >= reflect.Uint && typ.Kind() <= reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return err
		}
		v.SetUint(n)
		return nil
	case typ.Kind() == reflect.Float32 || typ.Kind() == reflect.Float64:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		v.SetFloat(f)
		return nil
	case typ.Kind() == reflect.Slice && typ.Elem().Kind() == reflect.String:
		parts := strings.Split(s, ",")
		for i, p := range parts {
			parts[i] = strings.TrimSpace(p)
		}
		v.Set(reflect.ValueOf(parts))
		return nil
	default:
		return fmt.Errorf("不支持的标量类型 %s", typ)
	}
}
