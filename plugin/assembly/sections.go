package assembly

import (
	"fmt"
	"reflect"
	"sort"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

// pluginsRoot and workloadsRoot are the two configuration roots the assembly
// package owns. Both are namespaces: each claims a prefix and accepts no key of
// its own, and every key beneath it belongs to a section declared in its own
// right -- one per selected Definition under plugins, one per declared workload
// under workloads.
const (
	pluginsRoot   = "plugins"
	workloadsRoot = "workloads"
)

// WorkloadConfig is the configuration one declared workload owns: the section
// "workloads.<key>".
//
// The workload's own placement constraints are deliberately absent. "replicas"
// and "exclusive" describe the cluster rather than one process, so making them
// configurable per process would let a single host reinterpret how many
// replicas may run or whether it must run alone, silently invalidating the
// placement every other process derived from the same declaration. What is left
// here is exactly what one process may decide about itself.
//
// Its bounds are checked by validateWorkloadConfig rather than by validation
// tags: this section is bound directly, without the config.Validate pass the
// plugin path runs, so a tag here would never execute.
type WorkloadConfig struct {
	// MaxGoroutines bounds how many managed tasks this process may run on
	// behalf of this workload at once. 0 means unbounded, which is the
	// behaviour of every workload that does not ask for a bound.
	MaxGoroutines int `yaml:"max_goroutines" default:"0"`
}

// WorkloadSection is one declared workload together with the configuration its
// own section holds.
type WorkloadSection struct {
	// Workload is the declaration, constraints included.
	Workload plugin.Workload
	// Enabled reports whether this process is allowed to carry the workload at
	// all. It is the framework-owned "enabled" flag, and false is a hard veto:
	// no placement source may override it, because excluding a process from a
	// role outright is a deployment decision rather than a placement one.
	Enabled bool
	// Config is the workload's own section, bound and validated.
	Config WorkloadConfig
}

// WorkloadConfigPath returns the configuration path one workload's section owns.
func WorkloadConfigPath(key plugin.WorkloadKey) string {
	return workloadsRoot + "." + key.String()
}

// ConfigSections describes the configuration each selected Definition owns, so
// that the configuration layer can resolve environment variables against real
// schemas and reject a top-level key nobody claims. It runs before the
// configuration tree exists, which is exactly why it takes only Bundles.
//
// It freezes the same Bundles BuildPlan will freeze again; freezing is pure and
// deterministic, and keeping it here avoids leaking a half-built catalog into
// the caller.
func ConfigSections(bundles []plugin.Bundle) ([]config.Section, error) {
	selections, err := freezeBundles(bundles)
	if err != nil {
		return nil, err
	}
	workloads, err := declaredWorkloads(bundles)
	if err != nil {
		return nil, err
	}
	sections := make([]config.Section, 0, len(selections)+len(workloads)+1)
	sections = append(sections, config.Section{
		Path:  pluginsRoot,
		Owner: "the assembly layer",
		Kind:  config.SectionNamespace,
	})
	for _, selection := range selections {
		definition := selection.descriptor
		kind := config.SectionTyped
		if definition.Cardinality == pluginmodel.MultipleInstances {
			kind = config.SectionInstanced
		}
		section := config.Section{
			Path:   definitionPath(definition),
			Owner:  fmt.Sprintf("plugin %q", definition.Key),
			Kind:   kind,
			Toggle: true,
		}
		if definition.Config != nil {
			section.Schema = definition.Config.Type
		}
		sections = append(sections, section)
	}

	// The workloads root is declared only when the composition declares a
	// workload. An always-present root would be a namespace claiming a prefix
	// that nothing beneath it answers for, so "workloads.sast" in a deployment
	// that has no such workload would be reported as an unowned key whose only
	// declared sibling is itself -- a diagnostic pointing at nothing.
	for _, workload := range workloads {
		sections = append(sections, config.Section{
			Path:   WorkloadConfigPath(workload.Key),
			Owner:  fmt.Sprintf("workload %q", workload.Key),
			Kind:   config.SectionTyped,
			Toggle: true,
			Schema: reflect.TypeOf(WorkloadConfig{}),
		})
	}
	if len(workloads) > 0 {
		sections = append(sections, config.Section{
			Path:  workloadsRoot,
			Owner: "the assembly layer",
			Kind:  config.SectionNamespace,
		})
	}
	return sections, nil
}

// ReadWorkloadSections returns every workload the composition declares, sorted
// by key, each with the configuration its own section holds.
//
// It is the single reader of the workloads root: the hosted set, the
// per-workload task budget and the diagnostics all ask this question, and
// answering it once means a section that is misspelled or mistyped fails the
// same way wherever it is read.
//
// Every declared workload is returned, including one whose section is absent or
// whose "enabled" is false; absence means enabled, exactly as it does for a
// plugin section. A workload nothing declares is reported by the configuration
// layer's ownership walk instead, which names the key and the sections beside
// it.
func ReadWorkloadSections(bundles []plugin.Bundle, env *config.Environment) ([]WorkloadSection, error) {
	workloads, err := declaredWorkloads(bundles)
	if err != nil {
		return nil, err
	}
	sections := make([]WorkloadSection, 0, len(workloads))
	for _, workload := range workloads {
		section, err := readWorkloadSection(workload, env)
		if err != nil {
			return nil, err
		}
		sections = append(sections, section)
	}
	return sections, nil
}

// readWorkloadSection binds one workload's configuration section.
//
// The section carries a real schema rather than a bare allowlist, so
// "max_goroutines" is validated, defaulted and reachable from an environment
// variable like any other configuration key, and a misspelled sibling beside it
// is rejected by the same strict bind every plugin section already goes through.
// "enabled" is the one key accepted and deliberately not decoded into the
// schema, because the framework owns it.
func readWorkloadSection(workload plugin.Workload, env *config.Environment) (WorkloadSection, error) {
	section := WorkloadSection{Workload: workload, Enabled: true}
	if env == nil {
		return section, nil
	}
	path := WorkloadConfigPath(workload.Key)
	enabled, err := sectionEnabled(env, path)
	if err != nil {
		return WorkloadSection{}, err
	}
	section.Enabled = enabled
	config := WorkloadConfig{}
	if err := env.BindWithOptions(path, &config, bindOptionsEnablingToggle); err != nil {
		return WorkloadSection{}, fmt.Errorf("xbc: workload %s failed to bind configuration %s: %w", workload.Key, path, err)
	}
	if err := validateWorkloadConfig(path, config); err != nil {
		return WorkloadSection{}, err
	}
	section.Config = config
	return section, nil
}

// bindOptionsEnablingToggle is the binding every framework-owned section with a
// schema and an enable flag uses: the schema is closed to unknown keys, and
// "enabled" is accepted without being a schema field.
var bindOptionsEnablingToggle = config.BindOptions{AllowedKeys: []string{"enabled"}}

// validateWorkloadConfig applies the constraints the schema tags cannot state.
//
// The zero values are all meaningful here -- no budget, no bound -- so the only
// rejected shape is a negative count, which no default tag can exclude because
// the default is 0 and the tag's own lower bound would then have to be 0 too.
func validateWorkloadConfig(path string, config WorkloadConfig) error {
	if config.MaxGoroutines < 0 {
		return fmt.Errorf(
			"xbc: %s.max_goroutines must not be negative, got %d; 0 leaves this workload's managed tasks unbounded",
			path, config.MaxGoroutines)
	}
	return nil
}

// declaredWorkloads returns the workloads the composition declares, sorted by
// key, after the declarations themselves have been cross-checked against every
// occurrence that claims membership.
//
// Both ConfigSections and BuildPlan discover the workloads through here, so the
// sections that exist and the workloads that can be hosted are derived from one
// reading of the same Bundles rather than from two that could disagree.
//
// It reads declarations only; the Definition-level validation of a Bundle is
// freezeBundles' job, and every caller runs that first.
func declaredWorkloads(bundles []plugin.Bundle) ([]plugin.Workload, error) {
	erased := make([]pluginmodel.Bundle, len(bundles))
	byDefinition := make(map[pluginmodel.Definition]pluginmodel.WorkloadKey)
	for index, bundle := range bundles {
		erased[index] = pluginmodel.Bundle(bundle)
		for _, entry := range pluginmodel.BundleEntries(erased[index]) {
			descriptor, ok := pluginmodel.DescribeDefinition(entry.Definition)
			if !ok {
				continue
			}
			byDefinition[entry.Definition] = descriptor.Workload
		}
	}
	if err := pluginmodel.ValidateWorkloads(erased, byDefinition); err != nil {
		return nil, err
	}
	var workloads []plugin.Workload
	for _, bundle := range bundles {
		workloads = append(workloads, plugin.BundleWorkloads(bundle)...)
	}
	return dedupeWorkloads(workloads), nil
}

// dedupeWorkloads collapses the per-Bundle declarations into one sorted list.
//
// Each Bundle reports its own declarations already sorted and de-duplicated, so
// only a key declared by more than one Bundle can still repeat here -- and
// ValidateWorkloads has already proven those agree, because two Bundles
// declaring one key with different placement is exactly the mistake it exists
// to catch.
func dedupeWorkloads(workloads []plugin.Workload) []plugin.Workload {
	if len(workloads) == 0 {
		return nil
	}
	byKey := make(map[plugin.WorkloadKey]plugin.Workload, len(workloads))
	keys := make([]plugin.WorkloadKey, 0, len(workloads))
	for _, workload := range workloads {
		if _, seen := byKey[workload.Key]; seen {
			continue
		}
		byKey[workload.Key] = workload
		keys = append(keys, workload.Key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	sorted := make([]plugin.Workload, len(keys))
	for index, key := range keys {
		sorted[index] = byKey[key]
	}
	return sorted
}
