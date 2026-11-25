package config

import (
	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/v2"
)

// String returns the value at path as a string; a non-string scalar is
// formatted, and an absent path yields "".
func (e *Environment) String(path string) string {
	if e == nil || e.k == nil {
		return ""
	}
	return e.k.String(path)
}

// Int returns the value at path as an int, or 0 when it is absent or not
// numeric.
func (e *Environment) Int(path string) int {
	if e == nil || e.k == nil {
		return 0
	}
	return e.k.Int(path)
}

// Bool returns the value at path as a bool, or false when it is absent or not
// a boolean.
func (e *Environment) Bool(path string) bool {
	if e == nil || e.k == nil {
		return false
	}
	return e.k.Bool(path)
}

// Float64 returns the value at path as a float64, or 0 when it is absent or
// not numeric.
func (e *Environment) Float64(path string) float64 {
	if e == nil || e.k == nil {
		return 0
	}
	return e.k.Float64(path)
}

// Strings returns the value at path as a string slice; an absent path yields
// an empty slice.
func (e *Environment) Strings(path string) []string {
	if e == nil || e.k == nil {
		return []string{}
	}
	return e.k.Strings(path)
}

// All returns an independent flat view of the merged tree: every leaf keyed by
// its full dotted path.
func (e *Environment) All() map[string]any {
	if e == nil || e.k == nil {
		return map[string]any{}
	}
	return e.k.All()
}

// DecodeOptions tunes Decode.
type DecodeOptions struct {
	// Tag is the struct tag naming the keys; "mapstructure" when empty.
	Tag string
	// MatchName decides whether a configuration key addresses a struct field.
	// Nil keeps the decoder's case-insensitive exact match.
	MatchName func(mapKey, fieldName string) bool
	// WeaklyTyped lets scalars convert across kinds, such as "5" into an int.
	WeaklyTyped bool
}

// Decode copies the subtree at path into out, a non-nil pointer. Unlike Bind
// it is lenient: it applies no schema, tolerates unknown keys, and does not
// write defaults back into the tree, which suits callers that read freeform
// sections.
func (e *Environment) Decode(path string, out any, options DecodeOptions) error {
	if e == nil || e.k == nil {
		return nil
	}
	tag := options.Tag
	if tag == "" {
		tag = "mapstructure"
	}
	return e.k.UnmarshalWithConf(path, out, koanf.UnmarshalConf{
		Tag: tag,
		DecoderConfig: &mapstructure.DecoderConfig{
			MatchName:        options.MatchName,
			Result:           out,
			WeaklyTypedInput: options.WeaklyTyped,
		},
	})
}
