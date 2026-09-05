package pluginmodel

import "github.com/xbcio/xbc/log"

// PublicDefinition converts the public package's defined handle type to this
// package's erased handle without exposing mutable descriptor state.
type PublicDefinition interface {
	~struct{ data *definitionData }
}

// DefinitionDescriptorOf returns a defensive copy for private assembly.
func DefinitionDescriptorOf(definition Definition) (DefinitionDescriptor, bool) {
	return DescribeDefinition(definition)
}

// NewFactoryContext creates one per-factory context for private assembly.
func NewFactoryContext(identity Identity, logger log.Logger, slots map[uint64][]ResolvedEntry) BuildContext {
	return NewBuildContext(identity, logger, slots)
}
