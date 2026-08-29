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
			err = fmt.Errorf("xbc: %s 的 %s 回调发生 panic：%s\n%s",
				callbackSubject(key, instance), callback, formatPanicValue(recovered), debug.Stack())
		}
	}()
	return fn(), nil
}

func formatPanicValue(value any) (text string) {
	defer func() {
		if recover() != nil {
			text = fmt.Sprintf("<%T：panic 值格式化失败>", value)
		}
	}()
	return fmt.Sprint(value)
}

func callbackSubject(key plugin.Key, instance string) string {
	return fmt.Sprintf("插件 %s（实例 %q）", key, plugin.NormalizeInstance(instance))
}

func callConfigPtr(inst *Instance, configurable plugin.Configurable) (any, error) {
	target, err := invokeCallback(inst.key, inst.instance, "ConfigPtr()", configurable.ConfigPtr)
	if err != nil {
		return nil, err
	}
	if err := validateConfigTarget(target); err != nil {
		return nil, fmt.Errorf("xbc: %s 的 ConfigPtr() 返回值无效：%w",
			callbackSubject(inst.key, inst.instance), err)
	}
	return target, nil
}

func validateConfigTarget(target any) error {
	if target == nil {
		return fmt.Errorf("必须返回非 nil 的 struct 指针，得到 <nil>")
	}

	v := reflect.ValueOf(target)
	if v.Kind() != reflect.Pointer {
		return fmt.Errorf("必须返回非 nil 的 struct 指针，得到 %T", target)
	}
	if v.IsNil() {
		return fmt.Errorf("必须返回非 nil 的 struct 指针，得到 %s(nil)", v.Type())
	}
	if v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("必须返回非 nil 的 struct 指针，得到 %s", v.Type())
	}
	return nil
}
