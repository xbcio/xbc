package config

import (
	"encoding"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/v2"
)

// envName maps a config path to its environment variable name:
// "plugins.gorm.default.dsn" -> "XBC_PLUGINS_GORM_DEFAULT_DSN". Dashes become
// underscores too, so that a hyphenated plugin key stays addressable and
// agrees with the spelling Universe resolves against.
func envName(prefix, path string) string {
	return prefix + envSegment(path)
}

// bind performs the strict configuration-binding chain for one subtree:
//  1. reject keys that are outside out's yaml-tag schema
//  2. unmarshal the subtree into out
//  3. overlay ENV values, driven by that same schema
//  4. apply default tags where neither config nor ENV supplied a value
//
// allowed contains section-relative framework keys (for example "enabled")
// which are accepted but deliberately not decoded into out.
func bind(k *koanf.Koanf, path string, out any, envPrefix string, allowed ...string) error {
	if k == nil {
		return fmt.Errorf("xbc: configuration not loaded, cannot bind %s", displayPath(path))
	}
	root, schema, err := schemaFor(out)
	if err != nil {
		return err
	}
	allowedSet, err := allowedPathSet(allowed)
	if err != nil {
		return err
	}

	section := k.Get(path)
	if unknown := schema.unknownFields(section, path, allowedSet); len(unknown) > 0 {
		return unknownFieldsError(path, unknown)
	}
	// mapstructure can squash an embedded *struct only after the pointer has
	// been allocated. Prepare temporary pointers for decoding, then restore
	// those whose inline schema had no file value so absent subtrees stay nil.
	temporaryInlinePointers := prepareInlinePointers(root, schema.Root, section)

	decoderConfig := &mapstructure.DecoderConfig{
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.TextUnmarshallerHookFunc(),
		),
		WeaklyTypedInput: true,
		SquashTagOption:  "inline",
	}
	unmarshalErr := k.UnmarshalWithConf(path, out, koanf.UnmarshalConf{
		Tag:           "yaml",
		DecoderConfig: decoderConfig,
	})
	for _, pointer := range temporaryInlinePointers {
		pointer.SetZero()
	}
	if unmarshalErr != nil {
		return fmt.Errorf("xbc: failed to bind configuration section %s: %w", displayPath(path), unmarshalErr)
	}

	items := schema.rootedLeaves(path)
	envSet := make(map[string]bool, len(items))
	for _, item := range items {
		name := envName(envPrefix, item.Path)
		raw, ok := os.LookupEnv(name)
		if !ok {
			continue
		}
		envSet[item.Path] = true
		field, ok := fieldByIndex(root, item.Index, true)
		if !ok {
			return fmt.Errorf("xbc: cannot locate configuration field %s", item.Path)
		}
		if err := setScalar(field, item.Type, raw); err != nil {
			// Neither the offending value nor the underlying parse error is
			// echoed: environment variables are where secrets live, every
			// strconv error quotes the input it rejected, and the variable
			// name plus the expected type is enough to act on.
			return fmt.Errorf("xbc: environment variable %s cannot be parsed as %s", name, item.Type)
		}
	}

	for _, item := range items {
		if envSet[item.Path] || k.Exists(item.Path) || !item.HasDefault {
			continue
		}
		field, ok := fieldByIndex(root, item.Index, true)
		if !ok {
			return fmt.Errorf("xbc: cannot locate configuration field %s", item.Path)
		}
		if err := setScalar(field, item.Type, item.Default); err != nil {
			return fmt.Errorf("xbc: field %s default tag %q cannot be parsed as %s: %w", item.Path, item.Default, item.Type, err)
		}
	}

	return nil
}

func allowedPathSet(paths []string) (map[string]struct{}, error) {
	allowed := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == "" || strings.HasPrefix(path, ".") || strings.HasSuffix(path, ".") || strings.Contains(path, "..") {
			return nil, fmt.Errorf("xbc: allowed configuration keys must be non-empty section relative paths, got %q", path)
		}
		allowed[path] = struct{}{}
	}
	return allowed, nil
}

func unknownFieldsError(path string, fields []string) error {
	fields = append([]string(nil), fields...)
	sort.Strings(fields)
	var b strings.Builder
	fmt.Fprintf(&b, "xbc: configuration section %s contains unknown field", displayPath(path))
	for _, field := range fields {
		b.WriteString("\n  ")
		b.WriteString(field)
	}
	return fmt.Errorf("%s", b.String())
}

func displayPath(path string) string {
	if path == "" {
		return "(root)"
	}
	return path
}

// setScalar parses s according to typ and stores it into v. Pointer scalar
// fields are allocated only when this function is actually called by an ENV
// or default hit.
func setScalar(v reflect.Value, typ reflect.Type, s string) error {
	if typ.Kind() == reflect.Pointer {
		if v.IsNil() {
			v.Set(reflect.New(typ.Elem()))
		}
		return setScalar(v.Elem(), typ.Elem(), s)
	}

	if typ == reflect.TypeOf(time.Duration(0)) {
		duration, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		v.SetInt(int64(duration))
		return nil
	}
	if v.CanAddr() {
		if unmarshaler, ok := v.Addr().Interface().(encoding.TextUnmarshaler); ok {
			return unmarshaler.UnmarshalText([]byte(s))
		}
	}

	switch typ.Kind() {
	case reflect.String:
		v.SetString(s)
		return nil
	case reflect.Bool:
		value, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		v.SetBool(value)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value, err := strconv.ParseInt(s, 10, typ.Bits())
		if err != nil {
			return err
		}
		v.SetInt(value)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value, err := strconv.ParseUint(s, 10, typ.Bits())
		if err != nil {
			return err
		}
		v.SetUint(value)
		return nil
	case reflect.Float32, reflect.Float64:
		value, err := strconv.ParseFloat(s, typ.Bits())
		if err != nil {
			return err
		}
		v.SetFloat(value)
		return nil
	case reflect.Slice:
		if typ.Elem().Kind() != reflect.String {
			break
		}
		if s == "" {
			v.Set(reflect.MakeSlice(typ, 0, 0))
			return nil
		}
		parts := strings.Split(s, ",")
		result := reflect.MakeSlice(typ, len(parts), len(parts))
		for i, part := range parts {
			result.Index(i).SetString(strings.TrimSpace(part))
		}
		v.Set(result)
		return nil
	}
	return fmt.Errorf("unsupported scalar type %s", typ)
}
