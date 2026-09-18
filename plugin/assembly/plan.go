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
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

// PlanOptions are the immutable inputs to metadata-only assembly planning.
type PlanOptions struct {
	Bundles []plugin.Bundle
	Env     *config.Environment
	Logger  log.Logger
	// Placement is this process's hosting decision. It is an input rather than
	// a decision taken while planning, because it decides which Definitions
	// exist here at all -- see Plan.Workloads. Its zero value hosts every
	// workload the configuration enables, which is what a caller with no
	// placement source to consult asks for.
	Placement plugin.Placement
}

// Plan is a fully validated, frozen application graph. It contains factories
// but has not called one and owns no runtime resources.
type Plan struct {
	definitionCount int
	instances       map[plugin.Identity]*plannedInstance
	order           []plugin.Identity
	disabled        []DisabledDefinition
	contractIndex   map[reflect.Type][]plugin.Identity
	placement       plugin.Placement
	workloads       []PlanWorkload
	workloadOf      map[plugin.Identity]plugin.WorkloadKey
}

// PlanWorkload is one workload the composition declares, together with this
// process's decision about it and the identities it put in the graph.
type PlanWorkload struct {
	// Workload is the declaration, placement constraints included.
	Workload plugin.Workload
	// Hosted reports whether this process carries the workload.
	Hosted bool
	// Identities are the instances the workload contributed to the plan, in
	// graph order. It is empty for an unhosted workload, and it is also empty
	// for a hosted one whose Definitions the configuration disabled -- the two
	// are told apart by Hosted and by DisabledDetail, because an unhosted
	// workload and a disabled plugin are decisions taken by different layers.
	Identities []plugin.Identity
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
	selectedAt string
	configPath string
	// workload is the workload this instance's Definition belongs to, or ""
	// when it belongs to none. It is the reconciled answer -- the one
	// pluginmodel.ValidateWorkloads proved the declaration and the occurrence
	// agree on -- so downstream attribution never has to guess which of the
	// two ways of claiming membership won.
	workload  plugin.WorkloadKey
	plan      pluginmodel.InstancePlan
	bindings  map[uint64][]plugin.Identity
	logger    log.Logger
	lifecycle lifecycleDescriptor
}

type lifecycleDescriptor struct {
	init        func(any, *plugin.Context) error
	migrate     func(any, *plugin.Context) error
	start       func(any, *plugin.Context) error
	openTraffic func(any, *plugin.Context) error
	stop        func(any, context.Context) error
	preStop     func(any, context.Context) error
}

// Order returns the canonical deterministic dependency order.
func (plan *Plan) Order() []plugin.Identity {
	if plan == nil {
		return nil
	}
	return append([]plugin.Identity(nil), plan.order...)
}

// Workloads returns every workload the composition declares, sorted by key,
// each with this process's hosting decision and the identities it contributed.
//
// An unhosted workload is reported with Hosted false and no identities rather
// than omitted: "this process does not carry sast" is the answer an operator
// running doctor is looking for, and a workload missing from the list could not
// be told apart from one the composition never declared.
func (plan *Plan) Workloads() []PlanWorkload {
	if plan == nil {
		return nil
	}
	out := make([]PlanWorkload, len(plan.workloads))
	for index, workload := range plan.workloads {
		out[index] = PlanWorkload{
			Workload:   workload.Workload,
			Hosted:     workload.Hosted,
			Identities: append([]plugin.Identity(nil), workload.Identities...),
		}
	}
	return out
}

// Placement returns the hosting decision this plan was built from.
func (plan *Plan) Placement() plugin.Placement {
	if plan == nil {
		return plugin.Placement{}
	}
	return plugin.Placement{
		Source: plan.placement.Source,
		Holder: plan.placement.Holder,
		Hosted: append([]plugin.WorkloadKey(nil), plan.placement.Hosted...),
		Notes:  append([]string(nil), plan.placement.Notes...),
	}
}

// WorkloadOf returns the workload identity belongs to. It reports false when
// identity belongs to no workload, and also when identity is not part of the
// plan at all.
//
// It is the attribution table resource accounting reads: a per-workload budget
// charges a managed task to the workload of the Plugin that submitted it, and
// that question is answered here rather than by re-deriving membership from the
// Bundles, so the answer always matches the graph that was actually built.
func (plan *Plan) WorkloadOf(identity plugin.Identity) (plugin.WorkloadKey, bool) {
	if plan == nil {
		return "", false
	}
	workload, ok := plan.workloadOf[identity.Normalized()]
	return workload, ok
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

// InstanceSelectedAt returns the composition site that first selected the
// Definition behind identity: the BundleOf call that introduced it, as an
// absolute file:line. It answers "who put this plugin in my graph", which the
// Definition's own declaration site cannot, because that site lives inside the
// plugin's own package no matter who selected it.
//
// Selecting the same Definition twice is legitimate -- an aggregate Bundle and
// an explicit selection routinely overlap -- and the first site wins. It
// returns "" when identity is not part of the plan.
func (plan *Plan) InstanceSelectedAt(identity plugin.Identity) string {
	if plan == nil {
		return ""
	}
	instance, exists := plan.instances[identity]
	if !exists {
		return ""
	}
	return instance.selectedAt
}

// InstanceInputs returns identity's declared inputs in declaration order, each
// with the producers wiring bound to it. It returns nil when identity is not
// part of the plan.
func (plan *Plan) InstanceInputs(identity plugin.Identity) []InputEdge {
	if plan == nil {
		return nil
	}
	instance, exists := plan.instances[identity]
	if !exists {
		return nil
	}
	edges := make([]InputEdge, 0, len(instance.plan.Inputs))
	for _, token := range instance.plan.Inputs {
		producers := append([]plugin.Identity(nil), instance.bindings[token.ID]...)
		sortIdentities(producers)
		edges = append(edges, InputEdge{
			Contract:  token.Type,
			Query:     describeQuery(token.Kind),
			Producers: producers,
		})
	}
	return edges
}

// InputQuery names how one declared input binds producers, in the vocabulary
// of the declaration that created it.
type InputQuery string

const (
	// QueryRef binds the one producer the declaration named by identity.
	QueryRef InputQuery = "ref"
	// QueryOne binds the single enabled exporter of a contract.
	QueryOne InputQuery = "one"
	// QueryOptional binds at most one enabled exporter, possibly none.
	QueryOptional InputQuery = "optional"
	// QueryMany binds every enabled exporter, possibly none.
	QueryMany InputQuery = "many"
)

// InputEdge is one declared input of one enabled instance together with the
// producers wiring bound to it.
//
// Producers is empty exactly when an optional or many query matched no enabled
// exporter. That outcome is deliberately legal: it raises no error, writes no
// log line, and is otherwise indistinguishable from a satisfied input. Keeping
// the edge and leaving Producers empty, rather than omitting the edge, is what
// lets a diagnostic say "you think this is wired, and it is not".
type InputEdge struct {
	Contract  reflect.Type
	Query     InputQuery
	Producers []plugin.Identity
}

func describeQuery(kind pluginmodel.QueryKind) InputQuery {
	switch kind {
	case pluginmodel.QueryRef:
		return QueryRef
	case pluginmodel.QueryOne:
		return QueryOne
	case pluginmodel.QueryOptional:
		return QueryOptional
	case pluginmodel.QueryMany:
		return QueryMany
	default:
		return InputQuery(fmt.Sprintf("query(%d)", kind))
	}
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
//
// The hosted set is applied before expansion and is the first thing the plan
// knows about a workload: a Definition belonging to a workload this process
// does not carry is dropped here and never enters the graph. The form matters.
// An unhosted Definition is not a DisabledDefinition -- it is absent -- because
// a disabled plugin is something the configuration decided about a plugin that
// exists in this process, and an unhosted workload is a plugin this process
// does not have. Reporting one as the other would put a workload's existence
// into diagnostics and snapshot diffs of a process that cannot construct it.
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
	workloads, err := declaredWorkloads(options.Bundles)
	if err != nil {
		return nil, err
	}
	hosted := hostedWorkloads(options.Placement, workloads)
	selections := make([]selectedDefinition, 0, len(definitions))
	// What the hosted set removes is remembered only as far as diagnostics
	// need it. See unhostedIndex for why nothing more is kept.
	var unhosted unhostedIndex
	for _, definition := range definitions {
		if definition.workload != "" && !hosted[definition.workload] {
			unhosted.record(definition)
			continue
		}
		selections = append(selections, definition)
	}
	instances, disabled, err := expandDefinitions(selections, options.Env, options.Logger)
	if err != nil {
		return nil, err
	}
	contractIndex := indexContracts(instances)
	order, err := wireGraph(instances, contractIndex, unhosted)
	if err != nil {
		return nil, err
	}
	for contract, identities := range contractIndex {
		contractIndex[contract] = orderSubset(order, identities)
	}
	placement := options.Placement
	if placement.Source == "" && placement.Hosted == nil {
		// The zero value means "host everything the configuration enables",
		// which is StaticPlacement's decision. Stamping it here keeps a
		// diagnostic from reporting an empty source for a plan that made a
		// real decision.
		placement.Source = placementSourceUnspecified
	}
	plan := &Plan{
		definitionCount: len(selections),
		instances:       instances,
		order:           order,
		disabled:        disabled,
		contractIndex:   contractIndex,
		placement:       placement,
		workloadOf:      make(map[plugin.Identity]plugin.WorkloadKey, len(instances)),
	}
	for identity, instance := range instances {
		if instance.workload != "" {
			plan.workloadOf[identity] = instance.workload
		}
	}
	plan.workloads = groupWorkloads(workloads, hosted, order, plan.workloadOf)
	return plan, nil
}

// placementSourceUnspecified labels a decision the caller left to the default.
// It names the default rather than the absence, because a reader asking "why
// does this process carry that" needs an answer and "unset" is not one.
const placementSourceUnspecified = "static (default)"

// hostedWorkloads resolves a placement decision into a set, treating the zero
// Placement as "everything the configuration enables".
//
// That reading of the zero value is what lets every caller that has no
// placement source -- a test, a diagnostic, an embedding host -- keep planning
// exactly as it did before placement existed. It stays a compatibility path
// rather than a fail-open one because the runtime refuses a resolved decision
// that names no source, so an answer a source actually produced can never reach
// here as the zero value.
func hostedWorkloads(placement plugin.Placement, declared []plugin.Workload) map[plugin.WorkloadKey]bool {
	hosted := make(map[plugin.WorkloadKey]bool, len(declared))
	if placement.Source == "" && placement.Hosted == nil {
		for _, workload := range declared {
			hosted[workload.Key] = true
		}
		return hosted
	}
	for _, key := range placement.Hosted {
		hosted[key] = true
	}
	return hosted
}

// groupWorkloads pairs every declared workload with its decision and with the
// identities it contributed, in graph order.
func groupWorkloads(
	declared []plugin.Workload,
	hosted map[plugin.WorkloadKey]bool,
	order []plugin.Identity,
	workloadOf map[plugin.Identity]plugin.WorkloadKey,
) []PlanWorkload {
	if len(declared) == 0 {
		return nil
	}
	identities := make(map[plugin.WorkloadKey][]plugin.Identity, len(declared))
	for _, identity := range order {
		if workload, ok := workloadOf[identity]; ok {
			identities[workload] = append(identities[workload], identity)
		}
	}
	out := make([]PlanWorkload, len(declared))
	for index, workload := range declared {
		out[index] = PlanWorkload{
			Workload:   workload,
			Hosted:     hosted[workload.Key],
			Identities: identities[workload.Key],
		}
	}
	return out
}

// selectedDefinition is one frozen Definition together with the composition
// site that selected it. The two origins answer different questions: the
// descriptor's own Origin is the Define call inside the plugin's package, while
// selectedAt is the BundleOf call in whoever chose to include it.
//
// workload is the reconciled membership: the Definition's own
// Options[P].Workload when it named one, and otherwise the workload the
// occurrence that introduced it carries. freezeBundles' call to
// ValidateWorkloads has already rejected any occurrence where the two disagree,
// so picking either one here cannot hide a conflict.
type selectedDefinition struct {
	descriptor pluginmodel.DefinitionDescriptor
	selectedAt string
	workload   plugin.WorkloadKey
}

func freezeBundles(bundles []plugin.Bundle) ([]selectedDefinition, error) {
	byHandle := make(map[pluginmodel.Definition]pluginmodel.BundleEntry)
	byKey := make(map[pluginmodel.Key]pluginmodel.BundleEntry)
	for _, publicBundle := range bundles {
		for _, entry := range pluginmodel.BundleEntries(pluginmodel.Bundle(publicBundle)) {
			// Selecting the same Definition again is expected usage, not a
			// mistake: an aggregate Bundle and an explicit selection overlap
			// routinely. The first selection site is the one kept, so that a
			// diagnostic names where the Definition entered the graph.
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

	definitions := make([]selectedDefinition, 0, len(byKey))
	for _, entry := range byKey {
		descriptor, _ := pluginmodel.DescribeDefinition(entry.Definition)
		definitions = append(definitions, selectedDefinition{
			descriptor: descriptor,
			selectedAt: entry.Origin,
			workload:   definitionWorkloadKey(descriptor, entry),
		})
	}
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].descriptor.Key < definitions[j].descriptor.Key })
	return definitions, nil
}

// definitionWorkloadKey reconciles the two ways a Definition claims workload
// membership. The Definition's own declaration wins when it named one, because
// that is the statement made by the package that owns the Definition; an
// occurrence's tag is what WorkloadOf applied when the workload package
// gathered it.
//
// pluginmodel.ValidateWorkloads has already rejected a Definition whose two
// claims disagree, so this function never has to choose between conflicting
// answers -- only between one answer and its absence.
func definitionWorkloadKey(descriptor pluginmodel.DefinitionDescriptor, entry pluginmodel.BundleEntry) plugin.WorkloadKey {
	if descriptor.Workload != "" {
		return descriptor.Workload
	}
	return entry.Workload
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

func expandDefinitions(selections []selectedDefinition, env *config.Environment, logger log.Logger) (map[plugin.Identity]*plannedInstance, []DisabledDefinition, error) {
	instances := make(map[plugin.Identity]*plannedInstance)
	var disabled []DisabledDefinition
	for _, selection := range selections {
		definition := selection.descriptor
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
				selectedAt: selection.selectedAt,
				configPath: instanceConfigPath(definition, identity),
				workload:   selection.workload,
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

// rejectKeysWithoutSchema keeps a Definition that declares no ConfigSpec as
// strict as one that does. Such a Definition still owns its section so it can
// be toggled, and that ownership is exactly what stops the configuration
// layer's unowned-key walk at the section boundary. Without this check a typo
// beneath it would be the one silently ignored key in the whole tree.
func rejectKeysWithoutSchema(definition pluginmodel.DefinitionDescriptor, identity plugin.Identity, env *config.Environment) error {
	path := instanceConfigPath(definition, identity)
	var unknown []string
	for name := range env.Sub(path) {
		if name == "enabled" {
			continue
		}
		unknown = append(unknown, path+"."+name)
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("xbc: plugin %s declares no configuration, so section %s accepts only enabled, got %s",
		identity, path, strings.Join(unknown, ", "))
}

func prepareConfig(definition pluginmodel.DefinitionDescriptor, identity plugin.Identity, env *config.Environment) (any, error) {
	if definition.Config == nil {
		return nil, rejectKeysWithoutSchema(definition, identity, env)
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

// dependencyEdge is one producer-before-consumer constraint, used as the key
// under which the graph remembers why that constraint exists.
type dependencyEdge struct {
	producer plugin.Identity
	consumer plugin.Identity
}

// wireGraph resolves every declared input, rejects the edges the dependency
// direction forbids, and returns the canonical construction order. unhosted
// carries no graph content: it is consulted only to explain a producer this
// process does not have, so that an absence caused by placement is not
// reported as an absence caused by the composition.
func wireGraph(instances map[plugin.Identity]*plannedInstance, contracts map[reflect.Type][]plugin.Identity, unhosted unhostedIndex) ([]plugin.Identity, error) {
	indegree := make(map[plugin.Identity]int, len(instances))
	outgoing := make(map[plugin.Identity]map[plugin.Identity]struct{}, len(instances))
	contractOf := make(map[dependencyEdge]reflect.Type)
	for identity := range instances {
		indegree[identity] = 0
		outgoing[identity] = make(map[plugin.Identity]struct{})
	}
	for consumer, instance := range instances {
		for _, token := range instance.plan.Inputs {
			matches, err := resolveToken(consumer, instance.workload, token, instances, contracts, unhosted)
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
					// One consumer may reach the same producer through
					// several tokens. The first one wins, which is stable
					// even though the outer loop walks a map: every edge
					// into this consumer is discovered while iterating this
					// consumer's own ordered Inputs slice.
					contractOf[dependencyEdge{producer: producer, consumer: consumer}] = token.Type
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
		return nil, cycleError(indegree, outgoing, contractOf)
	}
	return order, nil
}

// cycleError describes the residue Kahn's algorithm could not emit as one
// concrete cycle.
//
// The residue holds two different kinds of node: those that actually sit on a
// cycle, and those merely downstream of one, whose indegree can never reach
// zero either. Reporting the residue as a set therefore points at a region of
// the graph rather than at the loop that has to be broken, so the cycle path
// is recovered explicitly and the rest is reported separately as collateral.
func cycleError(
	indegree map[plugin.Identity]int,
	outgoing map[plugin.Identity]map[plugin.Identity]struct{},
	contractOf map[dependencyEdge]reflect.Type,
) error {
	stuck := make(map[plugin.Identity]struct{}, len(indegree))
	candidates := make([]plugin.Identity, 0, len(indegree))
	for identity, degree := range indegree {
		if degree > 0 {
			stuck[identity] = struct{}{}
			candidates = append(candidates, identity)
		}
	}
	sortIdentities(candidates)
	path := findCycle(candidates, stuck, outgoing)

	labels := make([]string, len(path))
	onCycle := make(map[plugin.Identity]struct{}, len(path))
	for index, identity := range path {
		labels[index] = identity.String()
		onCycle[identity] = struct{}{}
	}
	var message strings.Builder
	message.WriteString("xbc: plugin dependency cycle: ")
	message.WriteString(strings.Join(labels, " → "))
	for index := 0; index+1 < len(path); index++ {
		edge := dependencyEdge{producer: path[index], consumer: path[index+1]}
		if contract, known := contractOf[edge]; known {
			fmt.Fprintf(&message, "\n  %s requires %s from %s", edge.consumer, contract, edge.producer)
		}
	}

	blocked := make([]plugin.Identity, 0, len(candidates))
	for _, identity := range candidates {
		if _, cyclic := onCycle[identity]; !cyclic {
			blocked = append(blocked, identity)
		}
	}
	if len(blocked) > 0 {
		labels := make([]string, len(blocked))
		for index, identity := range blocked {
			labels[index] = identity.String()
		}
		fmt.Fprintf(&message, "\n  also blocked by this cycle: %s", strings.Join(labels, ", "))
	}
	return errors.New(message.String())
}

// findCycle walks the subgraph induced by stuck and returns the first cycle it
// reaches, as a path with its entry node repeated at the end.
//
// Both the choice of roots and the choice of successors are taken in canonical
// identity order rather than from a map, so a graph that contains several
// equally valid cycles still names the same one on every run.
func findCycle(
	roots []plugin.Identity,
	stuck map[plugin.Identity]struct{},
	outgoing map[plugin.Identity]map[plugin.Identity]struct{},
) []plugin.Identity {
	visited := make(map[plugin.Identity]struct{}, len(stuck))
	onStack := make(map[plugin.Identity]struct{}, len(stuck))
	var stack, found []plugin.Identity

	var visit func(identity plugin.Identity) bool
	visit = func(identity plugin.Identity) bool {
		visited[identity] = struct{}{}
		onStack[identity] = struct{}{}
		stack = append(stack, identity)
		for _, next := range stuckSuccessors(identity, stuck, outgoing) {
			if _, looping := onStack[next]; looping {
				entry := 0
				for index, member := range stack {
					if member == next {
						entry = index
						break
					}
				}
				found = append(append([]plugin.Identity(nil), stack[entry:]...), next)
				return true
			}
			if _, seen := visited[next]; !seen && visit(next) {
				return true
			}
		}
		delete(onStack, identity)
		stack = stack[:len(stack)-1]
		return false
	}

	for _, root := range roots {
		if _, seen := visited[root]; seen {
			continue
		}
		if visit(root) {
			return found
		}
	}
	return nil
}

func stuckSuccessors(
	identity plugin.Identity,
	stuck map[plugin.Identity]struct{},
	outgoing map[plugin.Identity]map[plugin.Identity]struct{},
) []plugin.Identity {
	successors := make([]plugin.Identity, 0, len(outgoing[identity]))
	for consumer := range outgoing[identity] {
		if _, blocked := stuck[consumer]; blocked {
			successors = append(successors, consumer)
		}
	}
	sortIdentities(successors)
	return successors
}

// ── dependency direction ────────────────────────────────────────────────
//
// The hosted set decides which Definitions exist in this process, so the
// direction of a dependency is not a policy that could have been chosen
// differently: it is a consequence of placement. Four rules follow, and all
// four are enforced here, before any factory runs.
//
//	workload -> unowned plugin                        allowed
//	unowned plugin -> workload, by Collect/Optional   allowed
//	unowned plugin -> workload, by One/Ref            error
//	workload -> a different workload                  error
//
// The first two are allowed for the same reason. An unowned plugin is present
// in every process shape, so depending on one cannot be affected by placement.
// And a query that tolerates zero candidates degrades to "not wired here"
// rather than to a failed startup, which is what web collecting
// RouteContributor and health collecting Contributor both rely on. OptionalOne
// is allowed alongside Collect for exactly that reason, and not because it is
// harmless: an optional dependency on a workload's producer is still a
// dependency whose satisfaction placement decides, but finding nothing is a
// legal outcome of the declaration, so nothing breaks and no rule is needed.
//
// The two errors are one rule seen from either end: a dependency whose
// existence is decided by placement would let the process shape, rather than
// the composition, decide whether the graph wires. RequireOne is the sharpest
// case -- an unhosted workload's producer is simply absent, so "found none"
// would name the symptom and hide the cause -- but RefTo is the quieter one,
// and it is the reason this check cannot live in the contract index alone.
// QueryRef looks its target up by key and never consults the index, so a guard
// built on the index would leave plugin.RefTo[T](key) as public API that walks
// straight around the rule.

// unhostedIndex records what the hosting decision removed before it could
// enter the graph, reduced to the two questions a diagnostic asks: which
// workload a plugin key belongs to, and which workloads declared a contract.
//
// It holds no Definitions. Nothing downstream may act on an unhosted
// Definition -- the entire point of deciding placement before planning is that
// an unhosted workload has no presence in this process -- so keeping the
// descriptors here would invite precisely the "construct it anyway" fallback
// the design forbids. A workload key is enough to explain an absence and
// cannot be mistaken for permission to construct anything.
type unhostedIndex struct {
	byKey      map[plugin.Key]plugin.WorkloadKey
	byContract map[reflect.Type][]plugin.WorkloadKey
}

// record adds one Definition the hosted set removed. Repeated calls for the
// same contract accumulate distinct workloads, because two unhosted workloads
// may both declare a producer of one contract and a diagnostic should name
// both rather than whichever happened to be recorded first.
func (index *unhostedIndex) record(definition selectedDefinition) {
	if definition.workload == "" {
		return
	}
	if index.byKey == nil {
		index.byKey = make(map[plugin.Key]plugin.WorkloadKey)
		index.byContract = make(map[reflect.Type][]plugin.WorkloadKey)
	}
	index.byKey[plugin.Key(definition.descriptor.Key)] = definition.workload

	declared := make([]reflect.Type, 0, len(definition.descriptor.Contracts)+1)
	declared = append(declared, definition.descriptor.Primary)
	for _, contract := range definition.descriptor.Contracts {
		declared = append(declared, contract.Type)
	}
	for _, contract := range declared {
		index.byContract[contract] = appendUniqueWorkload(index.byContract[contract], definition.workload)
	}
}

// suppliers returns the workloads that would have answered token had this
// process carried them. A Ref names one plugin key; every other form names a
// contract, so the two are indexed separately rather than by guessing which
// one a query meant.
func (index unhostedIndex) suppliers(token pluginmodel.InputToken) []plugin.WorkloadKey {
	if len(index.byKey) == 0 {
		return nil
	}
	if token.Kind == pluginmodel.QueryRef {
		workload, known := index.byKey[plugin.Key(token.Key)]
		if !known {
			return nil
		}
		return []plugin.WorkloadKey{workload}
	}
	return index.byContract[token.Type]
}

func appendUniqueWorkload(workloads []plugin.WorkloadKey, key plugin.WorkloadKey) []plugin.WorkloadKey {
	for _, existing := range workloads {
		if existing == key {
			return workloads
		}
	}
	return append(workloads, key)
}

// toleratesNoProducers reports whether a query kind is satisfied by finding
// nothing. Those are the only kinds an unowned plugin may aim at a workload's
// producer, because their zero-candidate outcome does not depend on placement:
// whether the producer is absent because this process does not carry its
// workload or because nothing declares the contract, the query resolves the
// same way.
//
// Only that side of QueryOptional is placement-independent, and the rest is a
// known residual. QueryOptional has a second failure mode QueryMany does not --
// ambiguity -- so when two workloads each export one contract an unowned
// OptionalOne consumes, carrying at most one of them resolves cleanly while
// carrying both fails, which is the hosted set deciding whether the process
// starts. It stays tolerated deliberately: refusing it here would reject an
// assembly that is perfectly determinate under static placement, and reporting
// it honestly belongs where candidates are resolved rather than in a direction
// check. The placement design records it as an open residual of this rule, so
// that an "is ambiguous" failure is read as possibly placement-induced.
func toleratesNoProducers(kind pluginmodel.QueryKind) bool {
	return kind == pluginmodel.QueryMany || kind == pluginmodel.QueryOptional
}

// instanceWorkload returns the workload that owns identity, or "" when it owns
// none. An identity that is not in the graph at all also reports "", which is
// correct for every caller here: the producers handed to checkDirection have
// already been resolved out of instances.
func instanceWorkload(instances map[plugin.Identity]*plannedInstance, identity plugin.Identity) plugin.WorkloadKey {
	if instance, ok := instances[identity]; ok {
		return instance.workload
	}
	return ""
}

// checkDirection applies the four rules to the producers one query actually
// bound.
//
// It runs after resolution rather than before, because three of the four rules
// are about who the producers turned out to be and cannot be answered from the
// token alone. The fourth -- a Ref naming a producer this process does not
// carry -- has no producer to inspect, so it is answered where the absence is
// discovered; see unhostedProducerError.
func checkDirection(
	consumer plugin.Identity,
	consumerWorkload plugin.WorkloadKey,
	token pluginmodel.InputToken,
	producers []plugin.Identity,
	instances map[plugin.Identity]*plannedInstance,
) error {
	for _, producer := range producers {
		producerWorkload := instanceWorkload(instances, producer)
		switch {
		case producerWorkload == "":
			// An unowned plugin is in every process shape, so a workload may
			// depend on one.
			continue
		case producerWorkload == consumerWorkload:
			// A workload depends on its own members constantly -- a dispatcher
			// requires its worker, an API collects its own contributors and
			// its own probes. That is one construction unit that is placed as
			// a whole, not two that might be separated.
			continue
		case consumerWorkload == "":
			if toleratesNoProducers(token.Kind) {
				continue
			}
			return unownedRequiresWorkloadError(consumer, token, producer, producerWorkload)
		default:
			return crossWorkloadError(consumer, consumerWorkload, token, producer, producerWorkload)
		}
	}
	return nil
}

// unhostedProducerError explains a resolution that found no producer because
// the only Definition that could have answered belongs to a workload this
// process does not carry.
//
// It returns nil when the absence has nothing to do with placement, which is
// what keeps every existing message intact: "found none" is a complete answer
// when nothing in the composition declares the contract. It stops being
// complete once placement exists, because then a Definition the composition
// root did select can still be absent from this process -- and the cause is a
// deployment decision the operator has to change, not a wiring mistake in the
// declaration they are reading.
func unhostedProducerError(
	consumer plugin.Identity,
	consumerWorkload plugin.WorkloadKey,
	token pluginmodel.InputToken,
	index unhostedIndex,
) error {
	absent := index.suppliers(token)
	if len(absent) == 0 {
		return nil
	}
	labels := make([]string, len(absent))
	for position, key := range absent {
		labels[position] = fmt.Sprintf("%q", key)
	}
	from := fmt.Sprintf("workload %s, which this process does not carry", strings.Join(labels, ", "))
	if consumerWorkload == "" {
		return fmt.Errorf(
			"xbc: plugin %s %s;\n  an unowned plugin cannot depend on a workload-scoped producer %s, because whether that producer exists is decided by placement rather than by the composition",
			consumer, requirementPhrase(token, from), dependencyForm(token))
	}
	return fmt.Errorf(
		"xbc: plugin %s %s;\n  workload %q cannot depend on workload %s directly, because whether the two are co-resident is decided by placement rather than by the graph",
		consumer, requirementPhrase(token, from), consumerWorkload, strings.Join(labels, ", "))
}

// unownedRequiresWorkloadError is the message for a plugin that belongs to no
// workload requiring one that does. It names the producer as well as its
// workload, because the fix is at the declaration site and the declaration
// names a plugin key.
func unownedRequiresWorkloadError(consumer plugin.Identity, token pluginmodel.InputToken, producer plugin.Identity, workload plugin.WorkloadKey) error {
	return fmt.Errorf(
		"xbc: plugin %s %s;\n  an unowned plugin cannot depend on a workload-scoped producer %s, because whether that producer exists is decided by placement rather than by the composition",
		consumer, requirementPhrase(token, fmt.Sprintf("plugin %s in workload %q", producer, workload)), dependencyForm(token))
}

// crossWorkloadError is the message for a workload depending on a different
// one. Co-residence is a lease outcome, so a direct edge between two workloads
// would hold in some processes and not others for reasons no declaration can
// express -- which is why both workload keys are named rather than only the
// missing one.
func crossWorkloadError(
	consumer plugin.Identity,
	consumerWorkload plugin.WorkloadKey,
	token pluginmodel.InputToken,
	producer plugin.Identity,
	producerWorkload plugin.WorkloadKey,
) error {
	return fmt.Errorf(
		"xbc: plugin %s %s;\n  workload %q cannot depend on workload %q directly, because whether the two are co-resident is decided by placement rather than by the graph",
		consumer, requirementPhrase(token, fmt.Sprintf("plugin %s in workload %q", producer, producerWorkload)), consumerWorkload, producerWorkload)
}

// requirementPhrase renders what a declaration asked for. It names the query
// form because the two forms are fixed in different places: a by-type
// dependency is changed by asking for a different contract, and a by-name one
// by naming a different key. An operator reading this has to know which edit is
// being asked of them.
func requirementPhrase(token pluginmodel.InputToken, from string) string {
	switch token.Kind {
	case pluginmodel.QueryRef:
		return fmt.Sprintf("requires %s by name from %s", token.Type, from)
	case pluginmodel.QueryOne:
		return fmt.Sprintf("requires exactly one %s by type from %s", token.Type, from)
	case pluginmodel.QueryOptional:
		return fmt.Sprintf("optionally requires %s by type from %s", token.Type, from)
	case pluginmodel.QueryMany:
		return fmt.Sprintf("collects %s by type from %s", token.Type, from)
	default:
		return fmt.Sprintf("requires %s from %s", token.Type, from)
	}
}

// dependencyForm names the query form in the line that states the rule, which
// is the half of the message a reader repeats when explaining the constraint
// to somebody else.
func dependencyForm(token pluginmodel.InputToken) string {
	if token.Kind == pluginmodel.QueryRef {
		return "by name"
	}
	return "by type"
}

func resolveToken(consumer plugin.Identity, consumerWorkload plugin.WorkloadKey, token pluginmodel.InputToken, instances map[plugin.Identity]*plannedInstance, contracts map[reflect.Type][]plugin.Identity, unhosted unhostedIndex) ([]plugin.Identity, error) {
	// The two branches below produce the same thing by different means, and
	// that difference is the whole reason the direction check runs after
	// resolution rather than before it: only one of them ever consults the
	// contract index.
	var candidates []plugin.Identity
	if token.Kind == pluginmodel.QueryRef {
		target := plugin.Identity{Plugin: plugin.Key(token.Key), Instance: token.Instance}.Normalized()
		instance, exists := instances[target]
		if !exists || !exports(instance, token.Type) {
			if !exists {
				// A Ref to a producer this process does not have is the one
				// direction failure that leaves nothing to inspect, so it is
				// explained here, where the absence still has a reason. The
				// generic message below is the fallback for a Ref whose target
				// is present and simply does not export the contract, which is
				// an ordinary wiring mistake and not a placement one.
				if err := unhostedProducerError(consumer, consumerWorkload, token, unhosted); err != nil {
					return nil, err
				}
			}
			return nil, fmt.Errorf("xbc: plugin %s requires %s from exact producer %s, but it is not enabled or does not export that contract", consumer, token.Type, target)
		}
		candidates = []plugin.Identity{target}
	} else {
		candidates = append([]plugin.Identity(nil), contracts[token.Type]...)
		switch token.Kind {
		case pluginmodel.QueryOne:
			if len(candidates) == 0 {
				if err := unhostedProducerError(consumer, consumerWorkload, token, unhosted); err != nil {
					return nil, err
				}
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
	}
	if err := checkDirection(consumer, consumerWorkload, token, candidates, instances); err != nil {
		return nil, err
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
