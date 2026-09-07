package main

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

// collectDescriptors evaluates every registered Definition accessor and
// returns the resulting descriptors sorted by plugin key. It only reads the
// already-built package-level Definition value; it never invokes a factory,
// a planner, or plugin/assembly's Construct.
func collectDescriptors() ([]pluginmodel.DefinitionDescriptor, error) {
	descriptors := make([]pluginmodel.DefinitionDescriptor, 0, len(definitionProviders))
	seen := make(map[string]string, len(definitionProviders))
	for _, provider := range definitionProviders {
		definition := provider()
		descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(definition))
		if !ok {
			return nil, fmt.Errorf("plugin-snapshots: a definition provider returned a zero-value plugin.Definition")
		}
		key := descriptor.Key.String()
		if origin, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("plugin-snapshots: duplicate plugin key %q declared at %s and %s", key, origin, descriptor.Origin)
		}
		seen[key] = descriptor.Origin
		descriptors = append(descriptors, descriptor)
	}
	sort.Slice(descriptors, func(i, j int) bool { return descriptors[i].Key < descriptors[j].Key })
	return descriptors, nil
}

// effectiveConfigPath mirrors plugin/assembly's own definitionPath
// convention: an explicit Options.ConfigPath wins, otherwise the
// configuration section is the conventional "plugins.<key>" that every
// plugin.WhenConfigured(...) activation in this repository already uses.
// Most plugin packages never set Options.ConfigPath explicitly (they rely on
// the convention through Activation instead), so falling back here is
// required for the snapshot to reflect the configuration section each plugin
// actually resolves against, not merely the possibly-empty raw field.
func effectiveConfigPath(descriptor pluginmodel.DefinitionDescriptor) string {
	if descriptor.ConfigPath != "" {
		return descriptor.ConfigPath
	}
	return "plugins." + descriptor.Key.String()
}

// qualifiedTypeName formats a reflect.Type as "<import path>.<name>" for a
// named/defined type. reflect.Type.String() is intentionally not used here:
// for a defined type it only prints the short package name (e.g.
// "session.Manager"), not the fully-qualified import path this snapshot's
// schema requires for a stable, unambiguous identity across packages that
// could share a short name.
//
// Contract types are normally interfaces, which are always named, but at
// least one plugin in this repository (transport/web/integrations/casbin)
// exports a concrete pointer type as an additional contract. Pointer types
// are themselves unnamed (PkgPath and Name are both empty), so this walks
// through any leading pointer indirection and qualifies the pointed-to named
// type instead, falling back to Type.String() only for the remaining
// unnamed-type shapes (slices, maps, funcs, ...) that do not occur among
// today's contracts or config types.
func qualifiedTypeName(t reflect.Type) string {
	depth := 0
	for t.Kind() == reflect.Pointer {
		depth++
		t = t.Elem()
	}
	prefix := strings.Repeat("*", depth)
	if t.PkgPath() == "" || t.Name() == "" {
		return prefix + t.String()
	}
	return prefix + t.PkgPath() + "." + t.Name()
}
