// Package pluginmodel contains the erased representation shared by the public
// plugin declarations and XBC's private assembly implementation. It is under
// internal/ so declaration internals cannot become an application API.
package pluginmodel

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
// Context-bearing stages accept any to keep this internal representation free
// of a dependency cycle with package plugin.
type LifecycleAdapters struct {
	Init        func(any, any) error
	Migrate     func(any, any) error
	Start       func(any, any) error
	OpenTraffic func(any, any) error
	Stop        func(any, context.Context) error
}

// InstancePlan is the complete side-effect-free plan for one instance.
type InstancePlan struct {
	Inputs  []InputToken
	Factory func(BuildContext) (any, error)
}

// DefinitionDescriptor is copied out of an opaque handle for private assembly.
type DefinitionDescriptor struct {
	Key         Key
	Origin      string
	Primary     reflect.Type
	Cardinality Cardinality
	Activation  Activation
	ConfigPath  string
	Contracts   []Contract
	Config      *ConfigDescriptor
	Plan        func(any) (InstancePlan, error)
	Lifecycle   LifecycleAdapters
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

// BundleEntry retains the composition origin that introduced a declaration.
type BundleEntry struct {
	Definition Definition
	Origin     string
}

// Bundle is a side-effect-free static collection with no runtime identity.
type Bundle struct {
	entries []BundleEntry
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
	for _, bundle := range bundles {
		entries = append(entries, bundle.entries...)
	}
	return Bundle{entries: entries}
}

func BundleEntries(bundle Bundle) []BundleEntry {
	return append([]BundleEntry(nil), bundle.entries...)
}

// ResolvedEntry is the erased representation of Entry[T] in a build slot.
type ResolvedEntry struct {
	Identity Identity
	Value    any
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
