package xbc

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/xbcio/xbc/log"
)

// expand is stage 2 of the assembly pipeline: it turns registered plugin
// prototypes (entries) into plugin instances, applying the enable-rule
// matrix (spec §6.4) and the multi-instance expansion shape (spec §6.5).
func (a *App) expand() ([]*instance, error) {
	seen := make(map[string]struct{}, len(a.entries))
	var out []*instance

	for _, e := range a.entries {
		// A duplicate plugin name is illegal regardless of multi/single --
		// letting it through would let two unrelated entries collapse onto
		// the same graph node id in stage 4 (see the id()/label() doc above).
		if _, dup := seen[e.name]; dup {
			return nil, fmt.Errorf("xbc: 插件名 %s 重复注册", e.name)
		}
		seen[e.name] = struct{}{}

		insts, err := a.expandEntry(e)
		if err != nil {
			return nil, err
		}
		out = append(out, insts...)
	}

	if err := a.checkOrphanSections(seen); err != nil {
		return nil, err
	}
	return out, nil
}

func (a *App) expandEntry(e entry) ([]*instance, error) {
	if e.multi {
		return a.expandMulti(e)
	}
	return a.expandSingle(e)
}

// expandSingle handles a plugin that did not declare MultiInstancer. It
// always produces at most one instance, at defaultInstance.
func (a *App) expandSingle(e entry) ([]*instance, error) {
	path := "plugins." + e.name
	enabled, err := a.sectionEnabled(path, a.cfg.Exists(path), e.src)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, nil
	}

	plugin := e.proto
	if e.src != sourceRegister {
		// Blank-import registrations never carry meaningful constructor
		// state (there was no call site to pass arguments), so there is no
		// reason to hold on to the package-level prototype -- always start
		// from a fresh zero value.
		plugin = clonePrototype(e.proto)
	}
	return []*instance{a.newInstance(plugin, e.name, defaultInstance, e.src)}, nil
}

// expandMulti handles a plugin that declared MultiInstance() == true.
func (a *App) expandMulti(e entry) ([]*instance, error) {
	path := "plugins." + e.name
	section := a.cfg.Sub(path)

	if section == nil {
		if e.src != sourceRegister {
			return nil, nil
		}
		return []*instance{a.newInstance(e.proto, e.name, defaultInstance, e.src)}, nil
	}

	pluginEnabled := true
	var names []string
	for k, v := range section {
		if _, ok := v.(map[string]any); ok {
			names = append(names, k)
			continue
		}
		if k != "enabled" {
			return nil, fmt.Errorf(
				"xbc: 多实例插件 %s 的配置节下 %q 不是实例（实例配置必须是映射）", e.name, k)
		}
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("xbc: %s.enabled 必须是布尔值，得到 %v", path, v)
		}
		pluginEnabled = b
	}
	if !pluginEnabled {
		return nil, nil
	}
	if len(names) == 0 {
		// The section exists (even as {}) but names no instances -- per the
		// single-instance enable rule extended to the multi-instance shape,
		// existence alone (regardless of src) means "enabled", so we fall
		// back to a lone default instance.
		names = []string{defaultInstance}
	}
	sort.Strings(names) // deterministic regardless of map iteration order

	var enabledNames []string
	for _, name := range names {
		sub, _ := section[name].(map[string]any)
		instEnabled := true
		if v, ok := sub["enabled"]; ok {
			b, ok := v.(bool)
			if !ok {
				return nil, fmt.Errorf("xbc: %s.%s.enabled 必须是布尔值，得到 %v", path, name, v)
			}
			instEnabled = b
		}
		if instEnabled {
			enabledNames = append(enabledNames, name)
		}
	}

	// Ruling R7: only an explicit Register that ends up with exactly one
	// enabled instance may reuse the constructor-supplied prototype.
	reusePrototype := e.src == sourceRegister && len(enabledNames) == 1
	if e.src == sourceRegister && len(enabledNames) > 1 {
		log.L().Warn(
			"多实例插件通过 app.Register 注册且展开出多个实例，构造参数不会带到各实例上，请确认插件零值可用",
			"plugin", e.name, "instances", len(enabledNames))
	}

	out := make([]*instance, 0, len(enabledNames))
	for _, name := range enabledNames {
		p := e.proto
		if !reusePrototype {
			p = clonePrototype(e.proto)
		}
		out = append(out, a.newInstance(p, e.name, name, e.src))
	}
	return out, nil
}

// sectionEnabled applies the enable-rule matrix (spec §6.4) for a config path
// that either exists or does not:
//
//	register + section absent  -> enabled (defaults)
//	register + section present -> enabled unless explicitly disabled
//	import   + section absent  -> disabled
//	import   + section present -> enabled unless explicitly disabled
func (a *App) sectionEnabled(path string, exists bool, src source) (bool, error) {
	if !exists {
		return src == sourceRegister, nil
	}
	v := a.cfg.Get(path + ".enabled")
	if v == nil {
		return true, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("xbc: %s.enabled 必须是布尔值，得到 %v", path, v)
	}
	return b, nil
}

// clonePrototype builds a fresh zero-value plugin of the same concrete type
// as proto. Ruling R7: a shallow copy would drag along embedded
// synchronization primitives (sync.Mutex and friends, which go vet flags),
// and there is no generically correct definition of a deep copy -- so any
// plugin instance that isn't the sole, explicitly-registered one must start
// from zero and get all of its state from configuration.
func clonePrototype(proto Plugin) Plugin {
	t := reflect.TypeOf(proto).Elem()
	return reflect.New(t).Interface().(Plugin)
}

// isMultiInstance reports whether p declared itself multi-instance. It is a
// plain type assertion rather than a stored flag on *instance, because the
// concrete plugin value is already sitting right there -- adding a
// duplicate bit of state would just be one more thing that could drift out
// of sync with the truth.
func isMultiInstance(p Plugin) bool {
	mi, ok := p.(MultiInstancer)
	return ok && mi.MultiInstance()
}

// newInstance builds one *instance together with its own *Context, and wires
// ctx/name back into the plugin's embedded Base (if any) so that
// Base.Ctx()/Log()/Name() work correctly from this point onward.
func (a *App) newInstance(p Plugin, name, inst string, src source) *instance {
	logger := log.L().With("plugin", name)
	if isMultiInstance(p) {
		logger = logger.With("instance", inst)
	}
	ctx := &Context{app: a, name: name, instance: inst, logger: logger}
	bindBase(p, ctx, name)
	return &instance{plugin: p, name: name, instance: inst, src: src, ctx: ctx}
}

// id returns the graph node id: "gorm" for a single-instance plugin,
// "gorm[readonly]" for a named instance other than default. The default
// instance never gets a bracket, whether the owning plugin is multi-instance
// or not -- that asymmetry is label()'s job, not id()'s.
func (i *instance) id() string {
	if i.instance == defaultInstance {
		return i.name
	}
	return i.name + "[" + i.instance + "]"
}

// label is the display form used in startup logs and error copy. It is the
// same as id(), except the default instance of a multi-instance plugin
// renders as "gorm[default]" so a reader can tell "single-instance plugin"
// apart from "multi-instance plugin's default instance" at a glance.
func (i *instance) label() string {
	if i.instance != defaultInstance {
		return i.id()
	}
	if isMultiInstance(i.plugin) {
		return i.name + "[" + defaultInstance + "]"
	}
	return i.name
}

// checkOrphanSections implements ruling R6: a plugins.<key> section with no
// matching registered plugin is a fatal startup error, not a warning. The
// entire point of this diagnostic is to catch "wrote config, forgot the
// import, silently did nothing" -- if the diagnostic itself failed silently
// too, that exact bug would slip through wearing a different hat.
//
// Only the plugins.* namespace is scanned. server./log./app. are reserved
// top-level namespaces handled elsewhere in the pipeline and are never
// treated as candidate plugin sections.
func (a *App) checkOrphanSections(registered map[string]struct{}) error {
	section := a.cfg.Sub("plugins")
	if section == nil {
		return nil
	}

	var orphans []string
	for k := range section {
		if _, ok := registered[k]; !ok {
			orphans = append(orphans, k)
		}
	}
	if len(orphans) == 0 {
		return nil
	}
	sort.Strings(orphans)

	var b strings.Builder
	for i, name := range orphans {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "xbc: plugins.%s 有配置但无对应插件\n  → 是否忘了 import github.com/xbcio/xbc/plugins/%s？", name, name)
	}
	return errors.New(b.String())
}
