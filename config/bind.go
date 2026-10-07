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
	// A map or slice the source supplies replaces the target's pre-filled
	// value rather than merging into it; scalar pre-fills stay untouched.
	zeroSourceCollections(root, section)

	decoderConfig := &mapstructure.DecoderConfig{
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			rejectFractionalFloat,
			textFromNumber,
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.TextUnmarshallerHookFunc(),
		),
		SquashTagOption: "inline",
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
		// This reads the process environment directly, while the environment
		// overlay resolves the same variable through Universe. Both name it
		// through envName/envSectionPrefix in universe.go, which is the one
		// place the spelling rule lives: two derivations of one name that
		// disagree would make the overlay merge a value bind never picks up.
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
		if !field.IsZero() {
			// The caller pre-filled this field (a plugin's ConfigSpec.Defaults,
			// typically). A pre-filled value is a decision the tag must not
			// overwrite, so the tag only supplies a value where the target is
			// still its zero value.
			continue
		}
		if err := setScalar(field, item.Type, item.Default); err != nil {
			return fmt.Errorf("xbc: field %s default tag %q cannot be parsed as %s: %w", item.Path, item.Default, item.Type, err)
		}
	}

	return nil
}

// zeroSourceCollections clears every map and slice the source section
// supplies, at any depth, so that decoding writes those values whole instead
// of merging them into what the caller pre-filled. Without it a user map
// keeps default keys it never mentioned -- an asynq "queues: {critical: 6}"
// would still carry the default "default" queue -- and a struct slice reuses
// pre-filled elements, so each decoded element inherits default fields the
// user left out. Scalars are deliberately left alone: their pre-filled values
// remain the caller's defaults.
func zeroSourceCollections(target reflect.Value, source any) {
	for target.IsValid() && (target.Kind() == reflect.Pointer || target.Kind() == reflect.Interface) {
		if target.IsNil() {
			// Nothing is pre-filled below a nil pointer; the decoder allocates a
			// fresh subtree, so there is no merge to undo.
			return
		}
		target = target.Elem()
	}
	if !target.IsValid() || target.Kind() != reflect.Struct {
		return
	}
	sourceMap, ok := stringMapValue(reflect.ValueOf(source))
	if !ok {
		return
	}

	structType := target.Type()
	for i := 0; i < structType.NumField(); i++ {
		field := structType.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name, inline, skip := yamlField(field)
		if skip {
			continue
		}
		fieldValue := target.Field(i)
		if inline {
			// An inline embedded struct shares the enclosing map's keys.
			zeroSourceCollections(fieldValue, source)
			continue
		}
		child, found := mapField(sourceMap, name)
		if !found {
			continue
		}
		childValue := dereferenceValue(child)
		if !childValue.IsValid() {
			continue
		}
		fieldType := dereference(field.Type)
		if isCollectionKind(fieldType) && isCollectionKind(childValue.Type()) {
			fieldValue.SetZero()
			continue
		}
		if isStructSchema(fieldType) {
			zeroSourceCollections(fieldValue, childValue.Interface())
		}
	}
}

func dereferenceValue(value reflect.Value) reflect.Value {
	for value.IsValid() && (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) {
		if value.IsNil() {
			return reflect.Value{}
		}
		value = value.Elem()
	}
	return value
}

func isCollectionKind(typ reflect.Type) bool {
	switch typ.Kind() {
	case reflect.Map, reflect.Slice, reflect.Array:
		return true
	default:
		return false
	}
}

// textFromNumber converts an integer source into the text of a string-kind
// type that parses its own syntax (encoding.TextUnmarshaler). An unquoted YAML
// integer such as xbc.runtime.max_procs: 4 is a number, while the field owning
// the key is deliberately a text type whose parser produces the diagnostic for
// every spelling of the setting, and the decimal spelling is exactly what the
// user wrote. A plain string field is not text of this kind and still refuses
// a number, as do booleans, whose only weak spellings ("1", "0") are not
// something anyone wrote.
func textFromNumber(from reflect.Type, to reflect.Type, data any) (any, error) {
	target := to
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	if target.Kind() != reflect.String {
		return data, nil
	}
	if !target.Implements(textUnmarshalerType) && !reflect.PointerTo(target).Implements(textUnmarshalerType) {
		return data, nil
	}
	switch from.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(reflect.ValueOf(data).Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(reflect.ValueOf(data).Uint(), 10), nil
	default:
		return data, nil
	}
}

// rejectFractionalFloat refuses a fractional or out-of-range float offered
// for an integer field. mapstructure would truncate 2.9 to 2 and wrap a large
// value silently; strict binding must neither. YAML numbers reach a schema
// through this path, while environment values are parsed into their final
// type before decoding and never take it.
func rejectFractionalFloat(from reflect.Type, to reflect.Type, data any) (any, error) {
	if from.Kind() != reflect.Float32 && from.Kind() != reflect.Float64 {
		return data, nil
	}
	target := to
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	signed := true
	switch target.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		signed = false
	default:
		return data, nil
	}

	text := strconv.FormatFloat(reflect.ValueOf(data).Float(), 'f', -1, 64)
	if signed {
		value, err := strconv.ParseInt(text, 10, target.Bits())
		if err != nil {
			return nil, fmt.Errorf("%v is not a whole number that fits %s", data, target)
		}
		return value, nil
	}
	value, err := strconv.ParseUint(text, 10, target.Bits())
	if err != nil {
		return nil, fmt.Errorf("%v is not a whole number that fits %s", data, target)
	}
	return value, nil
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
