package xbc

import (
	"context"
	"fmt"

	"github.com/xbcio/xbc/internal/inject"
)

// initAll drives stage 5. For every instance, in topological order: inject
// its dependencies, call Init if implemented, harvest its declared
// products, then verify every product it claimed via Provides() actually
// landed in the registry.
//
// inst.ctx was already constructed and bound to the plugin's embedded Base
// back in stage 2 (newInstance, stage_expand.go) -- stage 5 does not mint a
// second Context. Reusing the exact same object here is what keeps
// Base.Ctx() and the ctx handed to Init() from ever drifting apart.
//
// Any failure rolls back every instance that has already completed Init
// successfully, in reverse order, and returns the original error untouched
// -- rollback must never replace the error that caused it.
func (a *App) initAll(insts []*instance) error {
	for _, inst := range insts {
		if err := injectInstance(a, inst); err != nil {
			a.rollback(insts)
			return err
		}

		if initer, ok := inst.plugin.(Initializer); ok {
			if err := initer.Init(inst.ctx); err != nil {
				a.rollback(insts)
				return fmt.Errorf("xbc: 插件 %s 初始化失败: %w", inst.label(), err)
			}
		}
		// Reaching this line means either Init succeeded, or the plugin
		// never implemented Initializer in the first place -- in both
		// cases there is nothing pending that would make a later Stop
		// call unsafe, so the instance is eligible for rollback/shutdown.
		inst.inited = true

		if err := harvestInstance(a, inst); err != nil {
			a.rollback(insts)
			return err
		}

		if err := validateManualProvides(a, inst); err != nil {
			a.rollback(insts)
			return err
		}
	}
	return nil
}

// injectInstance fills every xbc:"inject" field on inst from the registry.
//
// A non-optional miss here is a framework bug, not a user error: stage 4
// (resolve) is supposed to have already turned every hard dependency into a
// graph edge and aborted the boot if it couldn't be satisfied. If stage 5
// still can't find it, resolve's static analysis and the registry's runtime
// state have drifted apart -- crashing loudly beats letting a nil slip
// through to whichever plugin queries it first, far from where the real
// mistake was made.
func injectInstance(a *App, inst *instance) error {
	for _, spec := range inst.fields {
		if spec.Kind != inject.KindInject {
			continue
		}
		val, err := a.registry.lookup(spec.Type, normInstance(spec.Instance))
		if err != nil {
			if spec.Optional {
				continue
			}
			dep := Dep{Type: spec.Type, Instance: spec.Instance}
			return fmt.Errorf("xbc: 内部错误——插件 %s 注入 %s 失败，阶段 4 本应拦住这个缺失: %w",
				inst.label(), dep.String(), err)
		}
		if err := inject.Set(inst.plugin, spec, val); err != nil {
			return fmt.Errorf("xbc: 插件 %s 字段 %s 注入失败: %w", inst.label(), spec.Name, err)
		}
	}
	return nil
}

// harvestInstance reads every xbc:"provide" field back out of inst after
// Init has returned, and registers it under (field type, plugin's own
// instance name). A field that is still zero means Init forgot to set it --
// catching that here, instead of leaving it to surface as a nil-pointer
// panic in some unrelated downstream plugin's first query, is the entire
// point of this step.
func harvestInstance(a *App, inst *instance) error {
	for _, spec := range inst.fields {
		if spec.Kind != inject.KindProvide {
			continue
		}
		zero, err := inject.IsZero(inst.plugin, spec)
		if err != nil {
			return fmt.Errorf("xbc: 插件 %s 收割字段 %s 失败: %w", inst.label(), spec.Name, err)
		}
		if zero {
			return fmt.Errorf("xbc: 插件 %s 声明产出 %s，但 Init 后该字段仍为 nil\n  → 检查 Init 中是否忘记给 %s 字段赋值",
				inst.label(), spec.Type.String(), spec.Name)
		}
		val, err := inject.Value(inst.plugin, spec)
		if err != nil {
			return fmt.Errorf("xbc: 插件 %s 读取字段 %s 失败: %w", inst.label(), spec.Name, err)
		}
		a.registry.put(spec.Type, normInstance(inst.instance), val)
	}
	return nil
}

// validateManualProvides checks the other half of Provider: types declared
// via Provides() (as opposed to a "provide" tag) that the plugin was
// supposed to register itself with a manual xbc.Provide call inside Init.
// A declaration with nothing behind it is exactly as dangerous as a zero
// "provide" field, so it gets the same fail-fast treatment.
//
// This runs after harvestInstance on purpose: it checks what actually
// landed in the registry, not merely what Provides() claims -- harvest is
// the step that puts real values into the registry, so validation has to
// happen after it, against the registry itself, not against the
// declaration a second time.
func validateManualProvides(a *App, inst *instance) error {
	provider, ok := inst.plugin.(Provider)
	if !ok {
		return nil
	}
	for _, dep := range provider.Provides() {
		if _, err := a.registry.lookup(dep.Type, normInstance(inst.instance)); err != nil {
			return fmt.Errorf("xbc: 插件 %s 的 Provides() 声明产出 %s，但 Init 中没有调用 xbc.Provide 登记该类型",
				inst.label(), dep.Type.String())
		}
	}
	return nil
}

// rollback stops every instance whose Init has already completed
// successfully, in reverse topological order -- dependents before their
// dependencies, mirroring shutdown's own ordering. A Stop error or panic is
// logged and swallowed: the original failure that triggered the rollback is
// the one the user needs to see, and letting a second failure stomp on it
// would hide the real cause.
func (a *App) rollback(insts []*instance) {
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.Server.ShutdownTimeout)
	defer cancel()

	for i := len(insts) - 1; i >= 0; i-- {
		inst := insts[i]
		if !inst.inited {
			continue
		}
		stopInstanceSafely(ctx, inst)
	}
}

// stopInstanceSafely calls Stop if the plugin implements Closer, recovering
// from a panic and logging any error instead of propagating it -- rollback
// must run to completion for every remaining instance no matter what one
// misbehaving Stop does.
func stopInstanceSafely(ctx context.Context, inst *instance) {
	closer, ok := inst.plugin.(Closer)
	if !ok {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			inst.ctx.Log().Error("回滚时 Stop panic，已忽略并继续", "plugin", inst.label(), "panic", r)
		}
	}()
	if err := closer.Stop(ctx); err != nil {
		inst.ctx.Log().Error("回滚时 Stop 失败，已忽略并继续", "plugin", inst.label(), "error", err)
	}
}
