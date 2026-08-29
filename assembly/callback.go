package assembly

import (
	"fmt"
	"reflect"
	"runtime/debug"

	"github.com/xbcio/xbc/plugin"
)

// invokeCallback is the single boundary for assembly-time plugin callbacks.
// A plugin panic is data supplied by an extension, not a reason to tear down
// the process, so it is converted into an identity-rich diagnostic here.
func invokeCallback[T any](key plugin.Key, instance, callback string, fn func() T) (result T, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("xbc: %s %s callback panicked: %s\n%s",
				callbackSubject(key, instance), callback, formatPanicValue(recovered), debug.Stack())
		}
	}()
	return fn(), nil
}

func formatPanicValue(value any) (text string) {
	defer func() {
		if recover() != nil {
			text = fmt.Sprintf("<%T: failed to format panic value>", value)
		}
	}()
	return fmt.Sprint(value)
}

func callbackSubject(key plugin.Key, instance string) string {
	return fmt.Sprintf("plugin %s (instance %q)", key, plugin.NormalizeInstance(instance))
}

func callConfigPtr(inst *Instance, configurable plugin.Configurable) (any, error) {
	target, err := invokeCallback(inst.key, inst.instance, "ConfigPtr()", configurable.ConfigPtr)
	if err != nil {
		return nil, err
	}
	if err := validateConfigTarget(target); err != nil {
		return nil, fmt.Errorf("xbc: %s returned an invalid ConfigPtr() value: %w",
			callbackSubject(inst.key, inst.instance), err)
	}
	return target, nil
}

func validateConfigTarget(target any) error {
	if target == nil {
		return fmt.Errorf("must return non-nil struct pointer, got <nil>")
	}

	v := reflect.ValueOf(target)
	if v.Kind() != reflect.Pointer {
		return fmt.Errorf("must return non-nil struct pointer, got %T", target)
	}
	if v.IsNil() {
		return fmt.Errorf("must return non-nil struct pointer, got %s(nil)", v.Type())
	}
	if v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("must return non-nil struct pointer, got %s", v.Type())
	}
	return nil
}
