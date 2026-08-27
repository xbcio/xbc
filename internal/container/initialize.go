package container

import (
	"fmt"

	"github.com/xbcio/xbc/internal/container/inject"
	"github.com/xbcio/xbc/plugin"
)

// Inject fills every xbc:"inject" field on inst from the container's value
// registry.
//
// A non-optional miss here is a framework bug, not a user error: resolve()
// is supposed to have already turned every hard dependency into a graph
// edge and failed Assemble if it couldn't be satisfied. If Inject still
// can't find it, resolve's static analysis and the registry's runtime state
// have drifted apart -- crashing loudly with an "内部错误" beats letting a
// nil slip through to whichever plugin queries it first, far from where the
// real mistake was made.
func (c *Container) Inject(inst *Instance) error {
	for _, spec := range inst.fields {
		if spec.Kind != inject.KindInject {
			continue
		}
		val, err := c.registry.lookup(spec.Type, plugin.NormalizeInstance(spec.Instance))
		if err != nil {
			if spec.Optional {
				continue
			}
			dep := plugin.Dep{Type: spec.Type, Instance: spec.Instance}
			return fmt.Errorf("xbc: 内部错误——插件 %s 注入 %s 失败，依赖解析本应拦住这个缺失: %w",
				inst.Label(), dep.String(), err)
		}
		if err := inject.Set(inst.plugin, spec, val); err != nil {
			return fmt.Errorf("xbc: 插件 %s 字段 %s 注入失败: %w", inst.Label(), spec.Name, err)
		}
	}
	return nil
}

// Harvest reads every xbc:"provide" field back out of inst after Init has
// returned, registers it under (field type, plugin's own instance name),
// and then validates the other half of Provider: types declared via
// Provides() (as opposed to a "provide" tag) that the plugin was supposed
// to register itself with a manual plugin.Provide call inside Init.
//
// Folding that second check into Harvest rather than exposing it as its own
// Container method is a deliberate judgment call: the pinned
// Container API has no separate slot for it, and both checks are really the
// same question -- "did everything this instance claimed to produce
// actually land in the registry" -- just for the two different declaration
// paths (tag vs. Provides()).
func (c *Container) Harvest(inst *Instance) error {
	for _, spec := range inst.fields {
		if spec.Kind != inject.KindProvide {
			continue
		}
		zero, err := inject.IsZero(inst.plugin, spec)
		if err != nil {
			return fmt.Errorf("xbc: 插件 %s 收割字段 %s 失败: %w", inst.Label(), spec.Name, err)
		}
		if zero {
			return fmt.Errorf("xbc: 插件 %s 声明产出 %s，但 Init 后该字段仍为 nil\n  → 检查 Init 中是否忘记给 %s 字段赋值",
				inst.Label(), spec.Type.String(), spec.Name)
		}
		val, err := inject.Value(inst.plugin, spec)
		if err != nil {
			return fmt.Errorf("xbc: 插件 %s 读取字段 %s 失败: %w", inst.Label(), spec.Name, err)
		}
		c.registry.put(spec.Type, plugin.NormalizeInstance(inst.instance), val)
	}
	return validateManualProvides(c, inst)
}

// validateManualProvides checks the declaration cache populated during
// resolve rather than invoking Provides() again. It runs after the tag loop
// on purpose: a type declared through both channels can be satisfied by the
// harvested field, and every Provides() callback is called only once during
// an assembly.
func validateManualProvides(c *Container, inst *Instance) error {
	for _, dep := range inst.provides {
		if _, err := c.registry.lookup(dep.Type, plugin.NormalizeInstance(inst.instance)); err != nil {
			return fmt.Errorf("xbc: 插件 %s 的 Provides() 声明产出 %s，但 Init 中没有调用 xbc.Provide 登记该类型",
				inst.Label(), dep.Type.String())
		}
	}
	return nil
}
