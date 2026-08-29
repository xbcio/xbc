package assembly

import (
	"fmt"
	"reflect"

	"github.com/xbcio/xbc/assembly/inject"
	"github.com/xbcio/xbc/plugin"
)

// scanPluginFields adapts inject's deliberately strict reflection helper to
// Plugin's deliberately broad marker contract.
//
// Plugin is a zero-method marker, so any non-nil dynamic value is a valid live
// plugin. Values that cannot expose top-level struct fields have nothing for
// the xbc tag scanner to do and therefore contribute no field declarations.
// A struct value is likewise valid when it has no actionable xbc tags.
//
// Once a value does declare an inject/provide field, however, it must retain
// the existing pointer-to-struct shape: inject needs an addressable field and
// Harvest needs one stable live object. Rejecting a tagged struct value is
// safer than silently ignoring a declaration that can never be injected.
func scanPluginFields(value plugin.Plugin) ([]inject.FieldSpec, error) {
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		// expand rejects nil Factory results before resolve. Keep this helper
		// defensive for direct internal callers and preserve inject's diagnostic.
		return inject.Scan(value)
	}

	if rv.Kind() == reflect.Pointer && !rv.IsNil() && rv.Elem().Kind() == reflect.Struct {
		return inject.Scan(value)
	}

	if rv.Kind() == reflect.Struct && hasActionableXBCTag(rv.Type()) {
		return nil, fmt.Errorf("inject: 带 xbc tag 的插件必须是非 nil 的 struct 指针，收到 %T", value)
	}

	return nil, nil
}

func hasActionableXBCTag(typ reflect.Type) bool {
	for i := 0; i < typ.NumField(); i++ {
		if tag, ok := typ.Field(i).Tag.Lookup("xbc"); ok && tag != "-" {
			return true
		}
	}
	return false
}
