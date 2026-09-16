package main

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

// collectDescriptors expands every registered Bundle accessor and returns the
// resulting descriptors sorted by plugin key. It only reads static Bundle
// entries and Definition metadata; it never invokes a factory, a planner, or
// plugin/assembly's Construct.
func collectDescriptors() ([]pluginmodel.DefinitionDescriptor, error) {
	descriptors, err := collectDescriptorsFromBundles(bundleProviders)
	if err != nil {
		return nil, err
	}
	if err := rejectReservedKeys(descriptors, collectReservedKeys()); err != nil {
		return nil, err
	}
	return descriptors, nil
}

// collectReservedKeys returns the sorted, de-duplicated keys transports reserve
// for stages they assemble themselves.
func collectReservedKeys() []pluginmodel.Key {
	seen := make(map[pluginmodel.Key]struct{})
	keys := make([]pluginmodel.Key, 0, len(reservedKeyProviders))
	for _, provider := range reservedKeyProviders {
		for _, key := range provider() {
			reserved := pluginmodel.Key(key)
			if _, duplicate := seen[reserved]; duplicate {
				continue
			}
			seen[reserved] = struct{}{}
			keys = append(keys, reserved)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// rejectReservedKeys fails generation when a Definition claims a key a transport
// reserves. plugin/assembly's freeze only compares Definition keys against each
// other, so nothing else in the repository catches this: the symptom would
// otherwise be a duplicate middleware identity at Start, in whichever
// application happened to select both.
func rejectReservedKeys(descriptors []pluginmodel.DefinitionDescriptor, reserved []pluginmodel.Key) error {
	if len(reserved) == 0 {
		return nil
	}
	index := make(map[pluginmodel.Key]struct{}, len(reserved))
	for _, key := range reserved {
		index[key] = struct{}{}
	}
	for _, descriptor := range descriptors {
		if _, claimed := index[descriptor.Key]; claimed {
			return fmt.Errorf(
				"plugin-snapshots: plugin key %q is reserved by a transport for a stage it assembles itself, but %s declares a Definition for it",
				descriptor.Key, descriptor.Origin,
			)
		}
	}
	return nil
}

// collectDescriptorsFromBundles mirrors plugin/assembly's bundle freeze
// semantics: repeat occurrences of the same immutable Definition handle are
// harmless, while two distinct handles claiming one key are rejected with
// both declaration and Bundle-entry origins.
func collectDescriptorsFromBundles(providers []func() plugin.Bundle) ([]pluginmodel.DefinitionDescriptor, error) {
	descriptors := make([]pluginmodel.DefinitionDescriptor, 0, len(providers))
	byHandle := make(map[pluginmodel.Definition]pluginmodel.BundleEntry)
	byKey := make(map[pluginmodel.Key]pluginmodel.BundleEntry)
	for _, provider := range providers {
		for _, entry := range pluginmodel.BundleEntries(pluginmodel.Bundle(provider())) {
			if _, repeated := byHandle[entry.Definition]; repeated {
				continue
			}
			descriptor, ok := pluginmodel.DescribeDefinition(entry.Definition)
			if !ok {
				return nil, fmt.Errorf("plugin-snapshots: bundle %s contains a zero Definition", entry.Origin)
			}
			if previous, collision := byKey[descriptor.Key]; collision {
				previousDescriptor, _ := pluginmodel.DescribeDefinition(previous.Definition)
				return nil, fmt.Errorf(
					"plugin-snapshots: plugin key %q is claimed by different Definition handles\n  first: %s (included by %s)\n  second: %s (included by %s)",
					descriptor.Key, previousDescriptor.Origin, previous.Origin, descriptor.Origin, entry.Origin,
				)
			}
			byHandle[entry.Definition] = entry
			byKey[descriptor.Key] = entry
			descriptors = append(descriptors, descriptor)
		}
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
// least one plugin in this repository (transport/web/extensions/authorization/casbin)
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
