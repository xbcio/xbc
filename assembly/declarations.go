package assembly

import (
	"fmt"

	"github.com/xbcio/xbc/assembly/inject"
	"github.com/xbcio/xbc/plugin"
)

// mergeDeps combines xbc:"inject" fields and Dependencies(). Dynamic
// callback results are validated before resolve is allowed to use reflect
// types or graph identifiers from them.
func mergeDeps(inst *Instance) (plugin.Deps, error) {
	var out plugin.Deps
	for _, field := range inst.fields {
		if field.Kind != inject.KindInject {
			continue
		}
		dep := plugin.Dep{Type: field.Type, Instance: field.Instance, Optional: field.Optional}
		if err := validateConsumerDep(dep); err != nil {
			return plugin.Deps{}, fmt.Errorf("xbc: 插件 %s 的 xbc tag 字段 %s 声明无效：%w",
				inst.Label(), field.Name, err)
		}
		out.Types = append(out.Types, dep)
	}

	declarer, ok := inst.plugin.(plugin.Declarer)
	if !ok {
		return out, nil
	}
	explicit, err := invokeCallback(inst.key, inst.instance, "Dependencies()", declarer.Dependencies)
	if err != nil {
		return plugin.Deps{}, err
	}
	if err := validateDependencies(inst, explicit); err != nil {
		return plugin.Deps{}, err
	}
	out.Types = append(out.Types, explicit.Types...)
	out.Plugins = append(out.Plugins, explicit.Plugins...)
	out.After = append(out.After, explicit.After...)
	out.Before = append(out.Before, explicit.Before...)
	return out, nil
}

// mergeProvides combines xbc:"provide" fields and Provides(). Provides is
// invoked exactly here, once for each live instance in one assembly; the
// merged result is cached on Instance and reused during Harvest.
func mergeProvides(inst *Instance) ([]plugin.Dep, error) {
	var out []plugin.Dep
	for _, field := range inst.fields {
		if field.Kind != inject.KindProvide {
			continue
		}
		out = append(out, plugin.Dep{Type: field.Type})
	}

	provider, ok := inst.plugin.(plugin.Provider)
	if !ok {
		return out, nil
	}
	explicit, err := invokeCallback(inst.key, inst.instance, "Provides()", provider.Provides)
	if err != nil {
		return nil, err
	}
	if err := validateProvides(inst, explicit); err != nil {
		return nil, err
	}
	out = append(out, explicit...)
	return out, nil
}

func validateDependencies(inst *Instance, deps plugin.Deps) error {
	for index, dep := range deps.Types {
		if err := validateConsumerDep(dep); err != nil {
			return invalidCallbackResult(inst, "Dependencies()", fmt.Sprintf("Types[%d]", index), err)
		}
	}
	for index, ref := range deps.Plugins {
		if err := ref.Key().Validate(); err != nil {
			return invalidCallbackResult(inst, "Dependencies()", fmt.Sprintf("Plugins[%d].Key", index), err)
		}
		if instance := ref.InstanceName(); instance != "" {
			if err := plugin.ValidateInstanceName(instance); err != nil {
				return invalidCallbackResult(inst, "Dependencies()", fmt.Sprintf("Plugins[%d].Instance", index), err)
			}
		}
	}
	for index, key := range deps.After {
		if err := key.Validate(); err != nil {
			return invalidCallbackResult(inst, "Dependencies()", fmt.Sprintf("After[%d]", index), err)
		}
	}
	for index, key := range deps.Before {
		if err := key.Validate(); err != nil {
			return invalidCallbackResult(inst, "Dependencies()", fmt.Sprintf("Before[%d]", index), err)
		}
	}
	return nil
}

func validateConsumerDep(dep plugin.Dep) error {
	if dep.Type == nil {
		return fmt.Errorf("Type 不能为空")
	}
	if dep.Instance != "" {
		if err := plugin.ValidateInstanceName(dep.Instance); err != nil {
			return fmt.Errorf("Instance %q 无效：%w", dep.Instance, err)
		}
	}
	return nil
}

func validateProvides(inst *Instance, deps []plugin.Dep) error {
	for index, dep := range deps {
		field := fmt.Sprintf("[%d]", index)
		switch {
		case dep.Type == nil:
			return invalidCallbackResult(inst, "Provides()", field+".Type", fmt.Errorf("Type 不能为空"))
		case dep.Instance != "":
			return invalidCallbackResult(inst, "Provides()", field+".Instance",
				fmt.Errorf("Instance 不受支持；产物实例由当前插件实例决定"))
		case dep.Optional:
			return invalidCallbackResult(inst, "Provides()", field+".Optional",
				fmt.Errorf("Optional 不受支持；产出声明不能标记为可选"))
		}
	}
	return nil
}

func invalidCallbackResult(inst *Instance, callback, field string, err error) error {
	return fmt.Errorf("xbc: %s 的 %s 返回值 %s 无效：%w",
		callbackSubject(inst.key, inst.instance), callback, field, err)
}
