package main

import (
	"encoding/json"
	"sort"

	"github.com/xbcio/xbc/internal/pluginmodel"
)

// identitySnapshot is the top-level shape of identity-snapshot.json: every
// plugin's canonical identity, deliberately excluding anything (Origin,
// source file, line number) that would churn on an unrelated code move.
type identitySnapshot struct {
	Plugins []identityPlugin `json:"plugins"`
}

type identityPlugin struct {
	Key         string   `json:"key"`
	ConfigPath  string   `json:"config_path"`
	Cardinality string   `json:"cardinality"`
	Contracts   []string `json:"contracts"`
}

// defaultSnapshot is the top-level shape of default-snapshot.json: every
// plugin's effective ConfigSpec default, for plugins that declare one.
type defaultSnapshot struct {
	Plugins []defaultPlugin `json:"plugins"`
}

type defaultPlugin struct {
	Key        string `json:"key"`
	ConfigType string `json:"config_type"`
	Defaults   any    `json:"defaults"`
}

// buildIdentitySnapshot converts descriptors (already sorted by key) into the
// identity-snapshot.json document.
func buildIdentitySnapshot(descriptors []pluginmodel.DefinitionDescriptor) identitySnapshot {
	plugins := make([]identityPlugin, 0, len(descriptors))
	for _, descriptor := range descriptors {
		contracts := make([]string, 0, len(descriptor.Contracts))
		for _, contract := range descriptor.Contracts {
			contracts = append(contracts, qualifiedTypeName(contract.Type))
		}
		sort.Strings(contracts)
		plugins = append(plugins, identityPlugin{
			Key:         descriptor.Key.String(),
			ConfigPath:  effectiveConfigPath(descriptor),
			Cardinality: descriptor.Cardinality.String(),
			Contracts:   contracts,
		})
	}
	return identitySnapshot{Plugins: plugins}
}

// buildDefaultSnapshot converts descriptors (already sorted by key) into the
// default-snapshot.json document, skipping plugins with no ConfigSpec.
// descriptor.Config.Defaults is the pure closure plugin.DefineConfigured and
// plugin.DefinePlanned wrap around the plugin package's DefaultConfig; it
// performs no I/O, no factory execution, and no BuildContext access.
func buildDefaultSnapshot(descriptors []pluginmodel.DefinitionDescriptor) defaultSnapshot {
	plugins := make([]defaultPlugin, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if descriptor.Config == nil {
			continue
		}
		plugins = append(plugins, defaultPlugin{
			Key:        descriptor.Key.String(),
			ConfigType: qualifiedTypeName(descriptor.Config.Type),
			Defaults:   descriptor.Config.Defaults(),
		})
	}
	return defaultSnapshot{Plugins: plugins}
}

// marshalSnapshot renders v the same way scripts/plugin-migration-inventory
// marshals its own inventory: two-space indent plus a trailing newline. Go's
// default map-key sorting and struct field ordering already make this
// deterministic.
func marshalSnapshot(v any) ([]byte, error) {
	contents, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(contents, '\n'), nil
}
