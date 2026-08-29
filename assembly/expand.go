package assembly

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// expand turns the frozen definitions into live instances. Cardinality comes
// exclusively from Definition.Instances; Factory is never called as a probe.
func (c *Container) expand() ([]*Instance, []string, error) {
	defs := c.snapshot.Definitions()
	produced := make(map[plugin.Key]bool, len(defs))
	var out []*Instance
	for _, d := range defs {
		insts, err := c.expandDefinition(d)
		if err != nil {
			return nil, nil, err
		}
		if len(insts) > 0 {
			produced[d.Key] = true
		}
		out = append(out, insts...)
	}

	var disabled []string
	for _, d := range defs {
		if !produced[d.Key] {
			disabled = append(disabled, d.Key.String())
		}
	}
	sort.Strings(disabled)

	if err := c.checkOrphanSections(defs); err != nil {
		return nil, nil, err
	}
	return out, disabled, nil
}

// expandDefinition evaluates every enablement rule before constructing live
// values. Each enabled instance gets exactly one Factory call; disabled
// definitions and instances get none.
func (c *Container) expandDefinition(d plugin.Definition) ([]*Instance, error) {
	if path, required := d.Activation.RequiresConfigSection(); required && !c.env.Exists(path) {
		return nil, nil
	}

	path := "plugins." + d.Key.String()
	if d.Instances == plugin.MultipleInstances {
		return c.expandMultiple(d, path)
	}

	enabled, err := c.sectionEnabled(path)
	if err != nil || !enabled {
		return nil, err
	}
	inst, err := c.construct(d, defaultInstance)
	if err != nil {
		return nil, err
	}
	return []*Instance{inst}, nil
}

func (c *Container) sectionEnabled(path string) (bool, error) {
	if !c.env.Exists(path) {
		return true, nil
	}
	v := c.env.Get(path + ".enabled")
	if v == nil {
		return true, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("xbc: %s.enabled must be a boolean, got %v", path, v)
	}
	return b, nil
}

func (c *Container) expandMultiple(d plugin.Definition, path string) ([]*Instance, error) {
	section := c.env.Sub(path)
	if section == nil {
		inst, err := c.construct(d, defaultInstance)
		if err != nil {
			return nil, err
		}
		return []*Instance{inst}, nil
	}

	pluginEnabled := true
	var names []string
	for name, raw := range section {
		if _, ok := raw.(map[string]any); ok {
			if err := plugin.ValidateInstanceName(name); err != nil {
				return nil, fmt.Errorf(
					"xbc: multi-instance plugin %s's instance name %q is invalid: %w\n  → instance names can only use lowercase letters, numbers, underscores, and hyphens; an empty string key is almost always a configuration typo",
					d.Key, name, err)
			}
			names = append(names, name)
			continue
		}
		if name != "enabled" {
			return nil, fmt.Errorf(
				"xbc: multi-instance plugin %s's configuration section %q is not an instance (instance configuration must be a map)", d.Key, name)
		}
		b, ok := raw.(bool)
		if !ok {
			return nil, fmt.Errorf("xbc: %s.enabled must be a boolean, got %v", path, raw)
		}
		pluginEnabled = b
	}
	if !pluginEnabled {
		return nil, nil
	}
	if len(names) == 0 {
		names = []string{defaultInstance}
	}
	sort.Strings(names)

	var enabled []string
	for _, name := range names {
		sub, _ := section[name].(map[string]any)
		if raw, ok := sub["enabled"]; ok {
			b, ok := raw.(bool)
			if !ok {
				return nil, fmt.Errorf("xbc: %s.%s.enabled must be a boolean, got %v", path, name, raw)
			}
			if !b {
				continue
			}
		}
		enabled = append(enabled, name)
	}

	out := make([]*Instance, 0, len(enabled))
	for _, name := range enabled {
		inst, err := c.construct(d, name)
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	return out, nil
}

func (c *Container) construct(d plugin.Definition, instance string) (*Instance, error) {
	p, err := invokeCallback(d.Key, instance, "Factory()", d.Factory)
	if err != nil {
		return nil, err
	}
	if nilPlugin(p) {
		return nil, fmt.Errorf("xbc: plugin %s's Factory returned nil for instance %q", d.Key, plugin.NormalizeInstance(instance))
	}
	return c.newInstance(p, d.Key, instance, d.Instances == plugin.MultipleInstances), nil
}

func nilPlugin(p plugin.Plugin) bool {
	if p == nil {
		return true
	}
	v := reflect.ValueOf(p)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return v.IsNil()
	default:
		return false
	}
}

func (c *Container) newInstance(p plugin.Plugin, key plugin.Key, instance string, multiple bool) *Instance {
	logger := c.logger
	if logger == nil {
		logger = log.L()
	}
	logger = logger.With("plugin", key.String())
	if multiple {
		logger = logger.With("instance", plugin.NormalizeInstance(instance))
	}
	id := plugin.Identity{Plugin: key, Instance: instance}
	configPath := "plugins." + key.String()
	if multiple {
		configPath += "." + plugin.NormalizeInstance(instance)
	}
	ctx := plugin.NewRuntimeContext(c.host, id, config.Scope(c.env, configPath), logger)
	plugin.BindRuntimeContext(p, ctx, key)
	return &Instance{
		plugin:   p,
		key:      key,
		multiple: multiple,
		instance: plugin.NormalizeInstance(instance),
		ctx:      ctx,
	}
}

func (c *Container) checkOrphanSections(defs []plugin.Definition) error {
	section := c.env.Sub("plugins")
	if section == nil {
		return nil
	}

	known := make(map[string]bool, len(defs))
	for _, d := range defs {
		known[d.Key.String()] = true
	}

	var orphans []string
	for key := range section {
		if !known[key] {
			orphans = append(orphans, key)
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
		fmt.Fprintf(&b, "xbc: plugins.%s has configuration but no corresponding plugin\n  → Did you forget to import the corresponding provider/autoload package?", name)
	}
	return errors.New(b.String())
}
