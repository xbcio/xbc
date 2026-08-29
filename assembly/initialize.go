package assembly

import (
	"fmt"

	"github.com/xbcio/xbc/assembly/inject"
	"github.com/xbcio/xbc/plugin"
)

// Inject fills every xbc:"inject" field on inst from the Container's value
// registry.
//
// A non-optional miss here is a framework bug, not a user error: resolve()
// is supposed to have already turned every hard dependency into a graph
// edge and failed Assemble if it couldn't be satisfied. If Inject still
// can't find it, resolve's static analysis and the registry's runtime state
// have drifted apart -- crashing loudly with an "internal error" beats letting a
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
			return fmt.Errorf("xbc: internal error — plugin %s injection of %s failed, dependency resolution should have detected this missing dependency: %w",
				inst.Label(), dep.String(), err)
		}
		if err := inject.Set(inst.plugin, spec, val); err != nil {
			return fmt.Errorf("xbc: plugin %s field %s injection failed: %w", inst.Label(), spec.Name, err)
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
			return fmt.Errorf("xbc: plugin %s failed to harvest field %s: %w", inst.Label(), spec.Name, err)
		}
		if zero {
			return fmt.Errorf("xbc: plugin %s declared that it provides %s, but the field is still nil after Init\n  → Check whether Init assigns field %s",
				inst.Label(), spec.Type.String(), spec.Name)
		}
		val, err := inject.Value(inst.plugin, spec)
		if err != nil {
			return fmt.Errorf("xbc: plugin %s failed to read field %s: %w", inst.Label(), spec.Name, err)
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
			return fmt.Errorf("xbc: plugin %s's Provides() declared %s, but Init did not call xbc.Provide to register this type",
				inst.Label(), dep.Type.String())
		}
	}
	return nil
}
