package assembly

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/internal/pluginmodel"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// PlanOptions are the immutable inputs to metadata-only assembly planning.
type PlanOptions struct {
	Bundles []plugin.Bundle
	Env     *config.Environment
	Logger  log.Logger
}

// Plan is a fully validated, frozen application graph. It contains factories
// but has not called one and owns no runtime resources.
type Plan struct {
	definitionCount int
	instances       map[plugin.Identity]*plannedInstance
	order           []plugin.Identity
	disabled        []DisabledDefinition
	contractIndex   map[reflect.Type][]plugin.Identity
}

// DisabledDefinition records a Definition that the configuration expanded to
// no enabled instance, together with why. Reason names paths and decisions
// only, never configured values, so a diagnostic may print it verbatim.
type DisabledDefinition struct {
	Key    plugin.Key
	Path   string
	Reason string
}

type plannedInstance struct {
	identity   plugin.Identity
	definition pluginmodel.DefinitionDescriptor
	configPath string
	plan       pluginmodel.InstancePlan
	bindings   map[uint64][]plugin.Identity
	logger     log.Logger
	lifecycle  lifecycleDescriptor
}

type lifecycleDescriptor struct {
	init        func(any, *plugin.Context) error
	migrate     func(any, *plugin.Context) error
	start       func(any, *plugin.Context) error
	openTraffic func(any, *plugin.Context) error
	stop        func(any, context.Context) error
}

// Order returns the canonical deterministic dependency order.
func (plan *Plan) Order() []plugin.Identity {
	if plan == nil {
		return nil
	}
	return append([]plugin.Identity(nil), plan.order...)
}

// DefinitionCount returns the number of distinct canonical Definitions after
// Bundle deduplication.
func (plan *Plan) DefinitionCount() int {
	if plan == nil {
		return 0
	}
	return plan.definitionCount
}

// Disabled returns sorted Definitions that expanded to no enabled instance.
func (plan *Plan) Disabled() []plugin.Key {
	if plan == nil {
		return nil
	}
	keys := make([]plugin.Key, len(plan.disabled))
	for index, entry := range plan.disabled {
		keys[index] = entry.Key
	}
	return keys
}

// DisabledDetail returns the same Definitions as Disabled, each with the
// configuration path it watched and the reason it stayed off.
func (plan *Plan) DisabledDetail() []DisabledDefinition {
	if plan == nil {
		return nil
	}
	return append([]DisabledDefinition(nil), plan.disabled...)
}

// InstanceConfigPath returns the configuration path that backs identity, or ""
// when identity is not part of the plan.
func (plan *Plan) InstanceConfigPath(identity plugin.Identity) string {
	if plan == nil {
		return ""
	}
	instance, exists := plan.instances[identity]
	if !exists {
		return ""
	}
	return instance.configPath
}

// Contracts returns the identities exporting contract in graph order.
func (plan *Plan) Contracts(contract reflect.Type) []plugin.Identity {
	if plan == nil {
		return nil
	}
	return append([]plugin.Identity(nil), plan.contractIndex[contract]...)
}

// BuildPlan performs catalog freeze, configuration expansion and preparation,
// pure per-instance planning, contract resolution, and graph wiring. It never
// invokes a primary factory.
func BuildPlan(options PlanOptions) (*Plan, error) {
	if options.Env == nil {
		var err error
		options.Env, err = config.NewEnvironment(nil, "")
		if err != nil {
			return nil, err
		}
	}
	definitions, err := freezeBundles(options.Bundles)
	if err != nil {
		return nil, err
	}
	instances, disabled, err := expandDefinitions(definitions, options.Env, options.Logger)
	if err != nil {
		return nil, err
	}
	if err := rejectOrphanConfiguration(definitions, options.Env); err != nil {
		return nil, err
	}
	contractIndex := indexContracts(instances)
	order, err := wireGraph(instances, contractIndex)
	if err != nil {
		return nil, err
	}
	for contract, identities := range contractIndex {
		contractIndex[contract] = orderSubset(order, identities)
	}
	return &Plan{
		definitionCount: len(definitions),
		instances:       instances,
		order:           order,
		disabled:        disabled,
		contractIndex:   contractIndex,
	}, nil
}

func freezeBundles(bundles []plugin.Bundle) ([]pluginmodel.DefinitionDescriptor, error) {
	byHandle := make(map[pluginmodel.Definition]pluginmodel.BundleEntry)
	byKey := make(map[pluginmodel.Key]pluginmodel.BundleEntry)
	for _, publicBundle := range bundles {
		for _, entry := range pluginmodel.BundleEntries(pluginmodel.Bundle(publicBundle)) {
			if _, repeated := byHandle[entry.Definition]; repeated {
				continue
			}
			descriptor, ok := pluginmodel.DescribeDefinition(entry.Definition)
			if !ok {
				return nil, fmt.Errorf("xbc: bundle %s contains a zero Definition", entry.Origin)
			}
			if previous, collision := byKey[descriptor.Key]; collision {
				previousDescriptor, _ := pluginmodel.DescribeDefinition(previous.Definition)
				return nil, fmt.Errorf(
					"xbc: plugin key %q is claimed by different Definition handles\n  first: %s (included by %s)\n  second: %s (included by %s)",
					descriptor.Key, previousDescriptor.Origin, previous.Origin, descriptor.Origin, entry.Origin,
				)
			}
			if err := validateDefinition(descriptor); err != nil {
				return nil, err
			}
			byHandle[entry.Definition] = entry
			byKey[descriptor.Key] = entry
		}
	}

	definitions := make([]pluginmodel.DefinitionDescriptor, 0, len(byKey))
	for _, entry := range byKey {
		descriptor, _ := pluginmodel.DescribeDefinition(entry.Definition)
		definitions = append(definitions, descriptor)
	}
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Key < definitions[j].Key })
	return definitions, nil
}

func validateDefinition(definition pluginmodel.DefinitionDescriptor) error {
	if err := definition.Key.Validate(); err != nil {
		return fmt.Errorf("xbc: invalid Definition declared at %s: %w", definition.Origin, err)
	}
	if definition.Cardinality != pluginmodel.SingleInstance && definition.Cardinality != pluginmodel.MultipleInstances {
		return fmt.Errorf("xbc: plugin %q has invalid cardinality %d", definition.Key, definition.Cardinality)
	}
	if err := validateConfigPath(definition.ConfigPath); err != nil {
		return fmt.Errorf("xbc: plugin %q has invalid config path %q: %w", definition.Key, definition.ConfigPath, err)
	}
	if definition.Activation.Kind == pluginmodel.ActivationConfigured {
		if err := validateConfigPath(definition.Activation.Path); err != nil {
			return fmt.Errorf("xbc: plugin %q has invalid activation path %q: %w", definition.Key, definition.Activation.Path, err)
		}
	} else if definition.Activation.Kind != pluginmodel.ActivationAlways {
		return fmt.Errorf("xbc: plugin %q has invalid activation kind %d", definition.Key, definition.Activation.Kind)
	}
	if definition.Primary == nil || definition.Primary.Kind() == reflect.Interface {
		return fmt.Errorf("xbc: plugin %q primary result type must be concrete, got %v", definition.Key, definition.Primary)
	}
	if definition.Plan == nil {
		return fmt.Errorf("xbc: plugin %q has no planner/factory", definition.Key)
	}
	seenContracts := map[reflect.Type]string{definition.Primary: "automatic primary contract"}
	for _, contract := range definition.Contracts {
		if contract.Type == nil || contract.Type.Kind() != reflect.Interface {
			return fmt.Errorf("xbc: plugin %q additional contract must be an interface, got %v at %s", definition.Key, contract.Type, contract.Origin)
		}
		if !definition.Primary.AssignableTo(contract.Type) {
			return fmt.Errorf("xbc: plugin %q primary type %s is not assignable to contract %s declared at %s", definition.Key, definition.Primary, contract.Type, contract.Origin)
		}
		if previous, duplicate := seenContracts[contract.Type]; duplicate {
			return fmt.Errorf("xbc: plugin %q declares contract %s twice (%s and %s)", definition.Key, contract.Type, previous, contract.Origin)
		}
		seenContracts[contract.Type] = contract.Origin
	}
	if definition.Config != nil {
		if definition.Config.Type == nil {
			return fmt.Errorf("xbc: plugin %q has an invalid nil configuration type", definition.Key)
		}
		if definition.Config.Defaults == nil {
			return fmt.Errorf("xbc: plugin %q ConfigSpec.Defaults cannot be nil", definition.Key)
		}
	}
	if _, err := compileLifecycle(definition); err != nil {
		return err
	}
	return nil
}

func validateConfigPath(path string) error {
	if path == "" {
		return nil
	}
	for _, segment := range strings.Split(path, ".") {
		if err := pluginmodel.ValidateIdentifier("configuration path segment", segment); err != nil {
			return err
		}
	}
	return nil
}

func expandDefinitions(definitions []pluginmodel.DefinitionDescriptor, env *config.Environment, logger log.Logger) (map[plugin.Identity]*plannedInstance, []DisabledDefinition, error) {
	instances := make(map[plugin.Identity]*plannedInstance)
	var disabled []DisabledDefinition
	for _, definition := range definitions {
		path := definitionPath(definition)
		if definition.Activation.Kind == pluginmodel.ActivationConfigured && !env.Exists(definition.Activation.Path) {
			disabled = append(disabled, DisabledDefinition{
				Key:    plugin.Key(definition.Key),
				Path:   path,
				Reason: "activation path " + definition.Activation.Path + " is not configured",
			})
			continue
		}
		identities, reason, err := expandIdentities(definition, env)
		if err != nil {
			return nil, nil, err
		}
		if len(identities) == 0 {
			disabled = append(disabled, DisabledDefinition{
				Key:    plugin.Key(definition.Key),
				Path:   path,
				Reason: reason,
			})
			continue
		}
		lifecycle, _ := compileLifecycle(definition)
		for _, identity := range identities {
			prepared, err := prepareConfig(definition, identity, env)
			if err != nil {
				return nil, nil, err
			}
			instancePlan, err := invokePlanner(definition, identity, prepared)
			if err != nil {
				return nil, nil, err
			}
			instanceLogger := logger
			if instanceLogger == nil {
				instanceLogger = log.L()
			}
			instanceLogger = instanceLogger.With("plugin", identity.Plugin.String())
			if definition.Cardinality == pluginmodel.MultipleInstances {
				instanceLogger = instanceLogger.With("instance", identity.Instance)
			}
			instances[identity] = &plannedInstance{
				identity:   identity,
				definition: definition,
				configPath: instanceConfigPath(definition, identity),
				plan:       instancePlan,
				bindings:   make(map[uint64][]plugin.Identity),
				logger:     instanceLogger,
				lifecycle:  lifecycle,
			}
		}
	}
	sort.Slice(disabled, func(i, j int) bool { return disabled[i].Key < disabled[j].Key })
	return instances, disabled, nil
}

func conventionalDefinitionPath(definition pluginmodel.DefinitionDescriptor) string {
	return pluginsRoot + "." + definition.Key.String()
}

func definitionPath(definition pluginmodel.DefinitionDescriptor) string {
	if definition.ConfigPath != "" {
		return definition.ConfigPath
	}
	return conventionalDefinitionPath(definition)
}

// instanceConfigPath is the section one concrete instance binds against.
func instanceConfigPath(definition pluginmodel.DefinitionDescriptor, identity plugin.Identity) string {
	path := definitionPath(definition)
	if definition.Cardinality == pluginmodel.MultipleInstances {
		return path + "." + identity.Instance
	}
	return path
}

// expandIdentities turns one Definition's configuration section into the
// instances it declares. When it returns no identity, the returned reason
// explains which flag turned the Definition off.
func expandIdentities(definition pluginmodel.DefinitionDescriptor, env *config.Environment) ([]plugin.Identity, string, error) {
	path := definitionPath(definition)
	if definition.Cardinality == pluginmodel.SingleInstance {
		enabled, err := sectionEnabled(env, path)
		if err != nil {
			return nil, "", err
		}
		if !enabled {
			return nil, path + ".enabled is false", nil
		}
		return []plugin.Identity{{Plugin: plugin.Key(definition.Key), Instance: plugin.DefaultInstance}}, "", nil
	}
	section := env.Sub(path)
	if section == nil {
		return []plugin.Identity{{Plugin: plugin.Key(definition.Key), Instance: plugin.DefaultInstance}}, "", nil
	}
	pluginEnabled := true
	var names []string
	for name, raw := range section {
		if _, ok := raw.(map[string]any); ok {
			if err := plugin.ValidateInstanceName(name); err != nil {
				return nil, "", fmt.Errorf("xbc: multi-instance plugin %s has invalid instance %q: %w", definition.Key, name, err)
			}
			names = append(names, name)
			continue
		}
		if name != "enabled" {
			return nil, "", fmt.Errorf("xbc: multi-instance plugin %s configuration %q is not an instance map", definition.Key, name)
		}
		value, ok := raw.(bool)
		if !ok {
			return nil, "", fmt.Errorf("xbc: %s.enabled must be boolean, got %v", path, raw)
		}
		pluginEnabled = value
	}
	if !pluginEnabled {
		return nil, path + ".enabled is false", nil
	}
	if len(names) == 0 {
		names = []string{plugin.DefaultInstance}
	}
	sort.Strings(names)
	identities := make([]plugin.Identity, 0, len(names))
	for _, name := range names {
		sub, _ := section[name].(map[string]any)
		if raw, exists := sub["enabled"]; exists {
			enabled, ok := raw.(bool)
			if !ok {
				return nil, "", fmt.Errorf("xbc: %s.%s.enabled must be boolean, got %v", path, name, raw)
			}
			if !enabled {
				continue
			}
		}
		identities = append(identities, plugin.Identity{Plugin: plugin.Key(definition.Key), Instance: name}.Normalized())
	}
	if len(identities) == 0 {
		return nil, "every instance configured under " + path + " is disabled", nil
	}
	return identities, "", nil
}

func sectionEnabled(env *config.Environment, path string) (bool, error) {
	if !env.Exists(path) {
		return true, nil
	}
	value := env.Get(path + ".enabled")
	if value == nil {
		return true, nil
	}
	enabled, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("xbc: %s.enabled must be boolean, got %v", path, value)
	}
	return enabled, nil
}

func prepareConfig(definition pluginmodel.DefinitionDescriptor, identity plugin.Identity, env *config.Environment) (any, error) {
	if definition.Config == nil {
		return nil, nil
	}
	var raw any
	var panicValue any
	func() {
		defer func() { panicValue = recover() }()
		raw = definition.Config.Defaults()
	}()
	if panicValue != nil {
		return nil, fmt.Errorf("xbc: plugin %s ConfigSpec.Defaults panic: %v\n%s", identity, panicValue, debug.Stack())
	}
	if raw == nil {
		return nil, fmt.Errorf("xbc: plugin %s ConfigSpec.Defaults returned nil", identity)
	}
	configType := definition.Config.Type
	valueType := reflect.TypeOf(raw)
	if valueType != configType {
		return nil, fmt.Errorf("xbc: plugin %s ConfigSpec.Defaults returned %s, want %s", identity, valueType, configType)
	}
	pointer := reflect.New(configType)
	pointer.Elem().Set(reflect.ValueOf(raw))
	path := instanceConfigPath(definition, identity)
	if err := env.BindWithOptions(path, pointer.Interface(), config.BindOptions{
		AllowedKeys: []string{"enabled"},
	}); err != nil {
		return nil, fmt.Errorf("xbc: plugin %s failed to bind configuration %s: %w", identity, path, err)
	}
	if err := config.Validate(pointer.Interface(), path); err != nil {
		return nil, fmt.Errorf("xbc: plugin %s configuration validation failed: %w", identity, err)
	}
	prepared := pointer.Elem().Interface()
	if definition.Config.Prepare == nil {
		return prepared, nil
	}
	var result any
	var err error
	panicValue = nil
	func() {
		defer func() { panicValue = recover() }()
		result, err = definition.Config.Prepare(prepared)
	}()
	if panicValue != nil {
		return nil, fmt.Errorf("xbc: plugin %s configuration Prepare panic: %v\n%s", identity, panicValue, debug.Stack())
	}
	if err != nil {
		return nil, fmt.Errorf("xbc: plugin %s configuration Prepare failed: %w", identity, err)
	}
	if reflect.TypeOf(result) != configType {
		return nil, fmt.Errorf("xbc: plugin %s configuration Prepare returned %T, want %s", identity, result, configType)
	}
	return result, nil
}

func invokePlanner(definition pluginmodel.DefinitionDescriptor, identity plugin.Identity, prepared any) (plan pluginmodel.InstancePlan, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("xbc: plugin %s planner panic: %v\n%s", identity, recovered, debug.Stack())
		}
	}()
	plan, err = definition.Plan(prepared)
	if err != nil {
		return pluginmodel.InstancePlan{}, fmt.Errorf("xbc: plugin %s planning failed: %w", identity, err)
	}
	if plan.Factory == nil {
		return pluginmodel.InstancePlan{}, fmt.Errorf("xbc: plugin %s planner returned nil factory", identity)
	}
	seen := make(map[uint64]pluginmodel.InputToken)
	for _, token := range plan.Inputs {
		if token.ID == 0 || token.Type == nil {
			return pluginmodel.InstancePlan{}, fmt.Errorf("xbc: plugin %s plan contains invalid input token", identity)
		}
		if previous, exists := seen[token.ID]; exists {
			return pluginmodel.InstancePlan{}, fmt.Errorf("xbc: plugin %s declares input token %d twice (%s and %s)", identity, token.ID, previous.Origin, token.Origin)
		}
		seen[token.ID] = token
	}
	return plan, nil
}

func indexContracts(instances map[plugin.Identity]*plannedInstance) map[reflect.Type][]plugin.Identity {
	index := make(map[reflect.Type][]plugin.Identity)
	for identity, instance := range instances {
		index[instance.definition.Primary] = append(index[instance.definition.Primary], identity)
		for _, contract := range instance.definition.Contracts {
			index[contract.Type] = append(index[contract.Type], identity)
		}
	}
	for _, identities := range index {
		sort.Slice(identities, func(i, j int) bool { return plugin.CompareIdentity(identities[i], identities[j]) < 0 })
	}
	return index
}

func wireGraph(instances map[plugin.Identity]*plannedInstance, contracts map[reflect.Type][]plugin.Identity) ([]plugin.Identity, error) {
	indegree := make(map[plugin.Identity]int, len(instances))
	outgoing := make(map[plugin.Identity]map[plugin.Identity]struct{}, len(instances))
	for identity := range instances {
		indegree[identity] = 0
		outgoing[identity] = make(map[plugin.Identity]struct{})
	}
	for consumer, instance := range instances {
		for _, token := range instance.plan.Inputs {
			matches, err := resolveToken(consumer, token, instances, contracts)
			if err != nil {
				return nil, err
			}
			instance.bindings[token.ID] = append([]plugin.Identity(nil), matches...)
			for _, producer := range matches {
				if producer == consumer {
					return nil, fmt.Errorf("xbc: plugin %s input token %d (%s) creates a self-dependency", consumer, token.ID, token.Type)
				}
				if _, exists := outgoing[producer][consumer]; !exists {
					outgoing[producer][consumer] = struct{}{}
					indegree[consumer]++
				}
			}
		}
	}

	ready := make([]plugin.Identity, 0)
	for identity, degree := range indegree {
		if degree == 0 {
			ready = append(ready, identity)
		}
	}
	sortIdentities(ready)
	order := make([]plugin.Identity, 0, len(instances))
	for len(ready) > 0 {
		next := ready[0]
		ready = ready[1:]
		order = append(order, next)
		children := make([]plugin.Identity, 0, len(outgoing[next]))
		for child := range outgoing[next] {
			children = append(children, child)
		}
		sortIdentities(children)
		for _, child := range children {
			indegree[child]--
			if indegree[child] == 0 {
				ready = append(ready, child)
				sortIdentities(ready)
			}
		}
	}
	if len(order) != len(instances) {
		var cyclic []plugin.Identity
		for identity, degree := range indegree {
			if degree > 0 {
				cyclic = append(cyclic, identity)
			}
		}
		sortIdentities(cyclic)
		parts := make([]string, len(cyclic))
		for i, identity := range cyclic {
			parts[i] = identity.String()
		}
		return nil, fmt.Errorf("xbc: plugin dependency cycle involves %s", strings.Join(parts, ", "))
	}
	return order, nil
}

func resolveToken(consumer plugin.Identity, token pluginmodel.InputToken, instances map[plugin.Identity]*plannedInstance, contracts map[reflect.Type][]plugin.Identity) ([]plugin.Identity, error) {
	candidates := append([]plugin.Identity(nil), contracts[token.Type]...)
	if token.Kind == pluginmodel.QueryRef {
		target := plugin.Identity{Plugin: plugin.Key(token.Key), Instance: token.Instance}.Normalized()
		instance, exists := instances[target]
		if !exists || !exports(instance, token.Type) {
			return nil, fmt.Errorf("xbc: plugin %s requires %s from exact producer %s, but it is not enabled or does not export that contract", consumer, token.Type, target)
		}
		return []plugin.Identity{target}, nil
	}
	switch token.Kind {
	case pluginmodel.QueryOne:
		if len(candidates) == 0 {
			return nil, fmt.Errorf("xbc: plugin %s requires exactly one %s exporter, found none", consumer, token.Type)
		}
		if len(candidates) > 1 {
			return nil, ambiguityError(consumer, token, candidates)
		}
	case pluginmodel.QueryOptional:
		if len(candidates) > 1 {
			return nil, ambiguityError(consumer, token, candidates)
		}
	case pluginmodel.QueryMany:
		// Empty is valid and self is deliberately retained for graph validation.
	default:
		return nil, fmt.Errorf("xbc: plugin %s has input token %d with unknown query kind %d", consumer, token.ID, token.Kind)
	}
	return candidates, nil
}

func ambiguityError(consumer plugin.Identity, token pluginmodel.InputToken, candidates []plugin.Identity) error {
	labels := make([]string, len(candidates))
	for i, candidate := range candidates {
		labels[i] = candidate.String()
	}
	return fmt.Errorf("xbc: plugin %s input %s is ambiguous; candidates: %s", consumer, token.Type, strings.Join(labels, ", "))
}

func exports(instance *plannedInstance, contract reflect.Type) bool {
	if instance.definition.Primary == contract {
		return true
	}
	for _, exported := range instance.definition.Contracts {
		if exported.Type == contract {
			return true
		}
	}
	return false
}

func sortIdentities(identities []plugin.Identity) {
	sort.Slice(identities, func(i, j int) bool { return plugin.CompareIdentity(identities[i], identities[j]) < 0 })
}

func orderSubset(order, candidates []plugin.Identity) []plugin.Identity {
	set := make(map[plugin.Identity]struct{}, len(candidates))
	for _, candidate := range candidates {
		set[candidate] = struct{}{}
	}
	result := make([]plugin.Identity, 0, len(candidates))
	for _, identity := range order {
		if _, exists := set[identity]; exists {
			result = append(result, identity)
		}
	}
	return result
}

// rejectOrphanConfiguration reports configuration under the conventional
// plugins root that no selected Definition answers for. A Definition with a
// custom ConfigPath participates whenever that path also lives under the
// plugins root, so that "plugins.custom" is recognised rather than reported as
// an orphan; a ConfigPath rooted elsewhere claims its own top-level section
// and is covered by the Universe ownership check instead.
func rejectOrphanConfiguration(definitions []pluginmodel.DefinitionDescriptor, env *config.Environment) error {
	section := env.Sub(pluginsRoot)
	if section == nil {
		return nil
	}
	known := make(map[string]struct{}, len(definitions))
	for _, definition := range definitions {
		path := definitionPath(definition)
		if !strings.HasPrefix(path, pluginsRoot+".") {
			continue
		}
		child := strings.TrimPrefix(path, pluginsRoot+".")
		if index := strings.IndexByte(child, '.'); index >= 0 {
			child = child[:index]
		}
		known[child] = struct{}{}
	}
	var orphans []string
	for key := range section {
		if _, exists := known[key]; !exists {
			orphans = append(orphans, key)
		}
	}
	if len(orphans) == 0 {
		return nil
	}
	sort.Strings(orphans)
	var errs []error
	for _, orphan := range orphans {
		errs = append(errs, fmt.Errorf("xbc: %s.%s has configuration but no corresponding plugin", pluginsRoot, orphan))
	}
	return errors.Join(errs...)
}
