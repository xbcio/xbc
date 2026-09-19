// Package model contains the low-level, type-erased representation shared by
// plugin declarations and XBC's assembly implementation. Application code
// should use the typed facade in the parent plugin package.
package model

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/xbcio/xbc/log"
)

// Key is the stable configuration-facing identity of a Definition.
type Key string

func (k Key) String() string { return string(k) }

func (k Key) Validate() error { return ValidateIdentifier("plugin key", string(k)) }

// WorkloadKey is the stable, configuration-facing identity of a workload: a
// named group of Definitions a process carries as a unit or not at all. Like
// Key it is never derived from a Go package path or type name, so the same
// workload keeps its identity across deployments that restructure the source
// tree.
//
// It is the spelling that appears in configuration ("workloads.<key>") and the
// identity a placement source files a claim under, which is why it is validated
// with the same rule as a plugin Key rather than accepting any string.
type WorkloadKey string

func (k WorkloadKey) String() string { return string(k) }

func (k WorkloadKey) Validate() error { return ValidateIdentifier("workload key", string(k)) }

const DefaultInstance = "default"

func NormalizeInstance(instance string) string {
	if instance == "" {
		return DefaultInstance
	}
	return instance
}

func ValidateInstanceName(name string) error {
	return ValidateIdentifier("instance name", name)
}

func ValidateIdentifier(kind, value string) error {
	if value == "" {
		return fmt.Errorf("xbc: %s cannot be empty", kind)
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return fmt.Errorf("xbc: %s %q contains invalid character %q, only lowercase letters, digits, underscores, and hyphens are allowed", kind, value, r)
		}
	}
	return nil
}

// Identity names one enabled Definition instance.
type Identity struct {
	Plugin   Key
	Instance string
}

func (i Identity) Normalized() Identity {
	i.Instance = NormalizeInstance(i.Instance)
	return i
}

func (i Identity) String() string {
	i = i.Normalized()
	if i.Instance == DefaultInstance {
		return i.Plugin.String()
	}
	return i.Plugin.String() + "[" + i.Instance + "]"
}

func CompareIdentity(left, right Identity) int {
	left = left.Normalized()
	right = right.Normalized()
	if left.Plugin < right.Plugin {
		return -1
	}
	if left.Plugin > right.Plugin {
		return 1
	}
	if left.Instance < right.Instance {
		return -1
	}
	if left.Instance > right.Instance {
		return 1
	}
	return 0
}

func SortIdentities(identities []Identity) {
	sort.Slice(identities, func(i, j int) bool {
		return CompareIdentity(identities[i], identities[j]) < 0
	})
}

type Cardinality uint8

const (
	SingleInstance Cardinality = iota
	MultipleInstances
)

func (c Cardinality) String() string {
	switch c {
	case SingleInstance:
		return "single"
	case MultipleInstances:
		return "multiple"
	default:
		return fmt.Sprintf("cardinality(%d)", c)
	}
}

type ActivationKind uint8

const (
	ActivationAlways ActivationKind = iota
	ActivationConfigured
)

// Activation is immutable enablement metadata.
type Activation struct {
	Kind ActivationKind
	Path string
}

// QueryKind determines how an Input token binds producers.
type QueryKind uint8

const (
	QueryRef QueryKind = iota + 1
	QueryOne
	QueryOptional
	QueryMany
)

var nextTokenID atomic.Uint64

// InputToken is immutable query metadata. Per-App bindings never live here.
type InputToken struct {
	ID       uint64
	Kind     QueryKind
	Type     reflect.Type
	Key      Key
	Instance string
	Origin   string
}

func NewInputToken(kind QueryKind, typ reflect.Type, key Key, instance, origin string) InputToken {
	return InputToken{
		ID:       nextTokenID.Add(1),
		Kind:     kind,
		Type:     typ,
		Key:      key,
		Instance: NormalizeInstance(instance),
		Origin:   origin,
	}
}

// Contract is one static interface export. Primary concrete types are indexed
// separately and automatically.
type Contract struct {
	Type   reflect.Type
	Origin string
}

// ConfigDescriptor erases one ConfigSpec without erasing its validation order.
type ConfigDescriptor struct {
	Type     reflect.Type
	Defaults func() any
	Prepare  func(any) (any, error)
}

// LifecycleAdapters contains typed adapters erased by the public constructor.
// Context-bearing stages accept any to keep this low-level representation free
// of a dependency cycle with the parent plugin package.
//
// The field order is declaration order, not execution order: PreStop is
// appended so that adding it moved nothing, and it runs before Stop. Erased
// adapters are read by name everywhere they are used, so the two orders never
// have to agree.
type LifecycleAdapters struct {
	Init        func(any, any) error
	Migrate     func(any, any) error
	Start       func(any, any) error
	OpenTraffic func(any, any) error
	Stop        func(any, context.Context) error
	PreStop     func(any, context.Context) error
}

// InstancePlan is the complete side-effect-free plan for one instance.
type InstancePlan struct {
	Inputs  []InputToken
	Factory func(BuildContext) (any, error)
}

// DefinitionDescriptor is copied out of an opaque handle for assembly.
type DefinitionDescriptor struct {
	Key         Key
	Origin      string
	Primary     reflect.Type
	Cardinality Cardinality
	Activation  Activation
	ConfigPath  string
	// Workload is the workload this Definition declares itself a member of, or
	// "" when it declares none. It is the escape hatch for a member whose
	// Definition lives in another module and therefore cannot be gathered by
	// plugin.WorkloadOf.
	Workload  WorkloadKey
	Contracts []Contract
	Config    *ConfigDescriptor
	Plan      func(any) (InstancePlan, error)
	Lifecycle LifecycleAdapters
}

type definitionData struct {
	descriptor DefinitionDescriptor
}

// Definition is an immutable declaration handle. Equality is pointer identity.
type Definition struct {
	data *definitionData
}

func NewDefinition(descriptor DefinitionDescriptor) Definition {
	descriptor.Contracts = append([]Contract(nil), descriptor.Contracts...)
	if descriptor.Config != nil {
		copy := *descriptor.Config
		descriptor.Config = &copy
	}
	return Definition{data: &definitionData{descriptor: descriptor}}
}

func DescribeDefinition(definition Definition) (DefinitionDescriptor, bool) {
	if definition.data == nil {
		return DefinitionDescriptor{}, false
	}
	descriptor := definition.data.descriptor
	descriptor.Contracts = append([]Contract(nil), descriptor.Contracts...)
	if descriptor.Config != nil {
		copy := *descriptor.Config
		descriptor.Config = &copy
	}
	return descriptor, true
}

func SameDefinition(left, right Definition) bool {
	return left.data != nil && left.data == right.data
}

// Workload is one declared workload's placement identity together with the
// cluster-level placement constraints it imposes.
//
// Both constraints describe the cluster rather than one process, which is
// exactly why neither is configurable per process: an override would let a
// single host reinterpret "how many replicas may run" or "must I run alone"
// and silently invalidate the placement every other process derived from the
// same declaration.
type Workload struct {
	// Key is the workload's stable identity. It names the configuration section
	// this workload is toggled by, and it is the identity a placement source
	// uses to name whatever it claims on the workload's behalf.
	Key WorkloadKey

	// Exclusive reports that a process holding this workload holds no other
	// workload. It exists for a workload with process-wide side effects, which
	// nothing sharing its process can be protected from.
	Exclusive bool

	// Replicas is a declaration the PlacementSource reads: a source that hands
	// out slots turns it into the number of processes that may hold this
	// workload at once, while the default StaticPlacement assigns no slots and
	// ignores it.
	Replicas int
}

// BundleEntry retains the composition origin that introduced a declaration.
type BundleEntry struct {
	Definition Definition
	Origin     string
	// Workload is the workload this occurrence belongs to, or "" when it
	// belongs to none. An occurrence belonging to no workload is present in
	// every process shape, which is what makes it safe for a workload to
	// depend on an unowned Definition and unsafe for an unowned Definition to
	// depend on a workload.
	Workload WorkloadKey
}

// Bundle is a side-effect-free static collection with no runtime identity. It
// collects both occurrences, each of which may name the workload it belongs
// to, and the workloads those occurrences' keys declare.
type Bundle struct {
	entries   []BundleEntry
	workloads []Workload
}

func NewBundle(origin string, definitions ...Definition) Bundle {
	entries := make([]BundleEntry, len(definitions))
	for i, definition := range definitions {
		entries[i] = BundleEntry{Definition: definition, Origin: origin}
	}
	return Bundle{entries: entries}
}

func CombineBundles(bundles ...Bundle) Bundle {
	var entries []BundleEntry
	var workloads []Workload
	for _, bundle := range bundles {
		entries = append(entries, bundle.entries...)
		workloads = append(workloads, bundle.workloads...)
	}
	return Bundle{entries: entries, workloads: workloads}
}

func BundleEntries(bundle Bundle) []BundleEntry {
	return append([]BundleEntry(nil), bundle.entries...)
}

// AssignWorkload returns a copy of bundle in which every occurrence names
// workload's key, with workload's declaration appended. The returned Bundle
// shares no slice with bundle, so neither can mutate the other's membership.
//
// It tags whatever occurrences the Bundle already holds and does not judge
// whether that is sensible: rejecting a Bundle that already belongs to a
// workload is plugin.WorkloadOf's job, because that is where the mistake --
// one workload nested inside another -- is actually written.
func AssignWorkload(bundle Bundle, workload Workload) Bundle {
	entries := make([]BundleEntry, len(bundle.entries))
	for i, entry := range bundle.entries {
		entry.Workload = workload.Key
		entries[i] = entry
	}
	workloads := make([]Workload, 0, len(bundle.workloads)+1)
	workloads = append(workloads, bundle.workloads...)
	workloads = append(workloads, workload)
	return Bundle{entries: entries, workloads: workloads}
}

// BundleWorkloads returns the workloads bundle declares, sorted by key.
//
// A key declared more than once is reported once, from its first occurrence.
// Collapsing here rather than failing is deliberate: this accessor has no way
// to report a conflict, and a conflicting redeclaration is a property of the
// whole composition rather than of one Bundle, so ValidateWorkloads owns it.
// Every consumer that must not act on an ambiguous declaration therefore runs
// ValidateWorkloads first.
func BundleWorkloads(bundle Bundle) []Workload {
	if len(bundle.workloads) == 0 {
		return nil
	}
	seen := make(map[WorkloadKey]bool, len(bundle.workloads))
	workloads := make([]Workload, 0, len(bundle.workloads))
	for _, workload := range bundle.workloads {
		if seen[workload.Key] {
			continue
		}
		seen[workload.Key] = true
		workloads = append(workloads, workload)
	}
	sort.Slice(workloads, func(i, j int) bool { return workloads[i].Key < workloads[j].Key })
	return workloads
}

// ValidateWorkloads reports workload declaration conflicts across a whole
// frozen composition.
//
// definitionWorkload maps each Definition to the workload key its own
// Options[P].Workload named, and holds "" for a Definition that named none.
// Passing it alongside the Bundles is what lets the two ways of claiming
// ownership -- plugin.WorkloadOf tagging occurrences, and a Definition naming
// a workload on itself -- be cross-checked instead of silently disagreeing.
//
// It rejects, naming both sides in every case:
//
//   - one Definition belonging to two different workload keys, whichever way
//     each ownership was declared;
//   - a Definition whose Options[P].Workload disagrees with the workload its
//     Bundle occurrence carries;
//   - one workload key declared twice with different placement;
//   - any ownership naming a workload key no declaration in the composition
//     declares.
//
// It deliberately does not judge placement across workloads: whether an
// exclusive workload may coexist with another is a property of one process's
// hosted set that only startup knows, not of the static composition.
func ValidateWorkloads(bundles []Bundle, definitionWorkload map[Definition]WorkloadKey) error {
	declared := make(map[WorkloadKey]Workload)
	for _, bundle := range bundles {
		for _, workload := range bundle.workloads {
			previous, exists := declared[workload.Key]
			if !exists {
				declared[workload.Key] = workload
				continue
			}
			if previous != workload {
				return fmt.Errorf(
					"xbc: workload %q is declared twice with different placement; first: exclusive=%t replicas=%d, second: exclusive=%t replicas=%d",
					workload.Key, previous.Exclusive, previous.Replicas, workload.Exclusive, workload.Replicas)
			}
		}
	}

	owned := make(map[Definition]WorkloadKey)
	for _, bundle := range bundles {
		for _, entry := range bundle.entries {
			member := entry.Workload
			if own := definitionWorkload[entry.Definition]; own != "" {
				if member != "" && member != own {
					return fmt.Errorf(
						"xbc: %s declares workload %q on itself but its Bundle occurrence belongs to workload %q; declare the ownership once",
						definitionLabel(entry.Definition), own, member)
				}
				member = own
			}
			if member == "" {
				continue
			}
			if previous, exists := owned[entry.Definition]; exists && previous != member {
				return fmt.Errorf(
					"xbc: %s belongs to workload %q and workload %q; a Definition belongs to at most one workload",
					definitionLabel(entry.Definition), previous, member)
			}
			owned[entry.Definition] = member
			if _, exists := declared[member]; !exists {
				return fmt.Errorf(
					"xbc: %s belongs to workload %q, which nothing in the composition declares; declare it with plugin.WorkloadOf",
					definitionLabel(entry.Definition), member)
			}
		}
	}
	return nil
}

// definitionLabel names a Definition handle in a diagnostic. A zero handle has
// no descriptor to name, so it is spelled out rather than left blank.
func definitionLabel(definition Definition) string {
	descriptor, ok := DescribeDefinition(definition)
	if !ok {
		return "a zero Definition"
	}
	return fmt.Sprintf("plugin %q", descriptor.Key)
}

// ResolvedEntry is the erased representation of Entry[T] in a build slot.
type ResolvedEntry struct {
	Identity Identity
	Value    any
	// Workload is the workload of the occurrence that produced Value, or ""
	// when that occurrence belongs to none.
	Workload WorkloadKey
}

type buildState struct {
	mu       sync.RWMutex
	active   bool
	consumer Identity
	logger   log.Logger
	slots    map[uint64][]ResolvedEntry
}

// BuildContext is valid only during one synchronous factory invocation. Every
// copy points at the same synchronized state so invalidation is race-safe.
type BuildContext struct {
	state *buildState
}

func NewBuildContext(consumer Identity, logger log.Logger, slots map[uint64][]ResolvedEntry) BuildContext {
	copied := make(map[uint64][]ResolvedEntry, len(slots))
	for id, entries := range slots {
		copied[id] = append([]ResolvedEntry(nil), entries...)
	}
	return BuildContext{state: &buildState{
		active:   true,
		consumer: consumer.Normalized(),
		logger:   logger,
		slots:    copied,
	}}
}

func (c BuildContext) Identity() Identity {
	state := c.requireActive("read identity")
	defer state.mu.RUnlock()
	return state.consumer
}

func (c BuildContext) Log() log.Logger {
	state := c.requireActive("read logger")
	defer state.mu.RUnlock()
	if state.logger == nil {
		return log.L()
	}
	return state.logger
}

func ReadBuildSlot(context BuildContext, token InputToken) []ResolvedEntry {
	state := context.requireActive("read input")
	defer state.mu.RUnlock()
	entries, ok := state.slots[token.ID]
	if !ok {
		panic(fmt.Sprintf("xbc: plugin %s used undeclared input token %d (%s)", state.consumer, token.ID, token.Type))
	}
	return append([]ResolvedEntry(nil), entries...)
}

func (c BuildContext) requireActive(operation string) *buildState {
	if c.state == nil {
		panic("xbc: zero BuildContext cannot " + operation)
	}
	c.state.mu.RLock()
	if !c.state.active {
		consumer := c.state.consumer
		c.state.mu.RUnlock()
		panic(fmt.Sprintf("xbc: plugin %s used BuildContext after its factory returned", consumer))
	}
	return c.state
}

func InvalidateBuildContext(context BuildContext) {
	if context.state == nil {
		return
	}
	context.state.mu.Lock()
	context.state.active = false
	context.state.slots = nil
	context.state.mu.Unlock()
}
