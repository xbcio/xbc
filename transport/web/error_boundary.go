package web

import (
	"fmt"
	"slices"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/ordering"
)

// ErrorOrder declares precedence among ErrorMapper Plugins. The first mapper
// that recognizes an error wins. References use the same typed strictness
// semantics as middleware order, but form an independent graph.
type ErrorOrder struct {
	After  []OrderRef
	Before []OrderRef
}

type errorBoundary struct {
	mappers []ErrorMapper
}

func (b *errorBoundary) Handler() gin.HandlerFunc { return Handle(OnError(b.mappers...)) }
func (*errorBoundary) Order() Order               { return Order{Phase: PhaseError} }

var errorMapperInput = plugin.Collect[ErrorMapper]()

var errorBoundaryDefinition = plugin.Define(
	ErrorBoundaryKey,
	func(ctx plugin.BuildContext) (*errorBoundary, error) {
		entries, err := orderErrorMappers(errorMapperInput.Get(ctx))
		if err != nil {
			return nil, err
		}
		mappers := make([]ErrorMapper, len(entries))
		for i, entry := range entries {
			mappers[i] = entry.Value
		}
		return &errorBoundary{mappers: mappers}, nil
	},
	plugin.Options[*errorBoundary]{
		Inputs: plugin.Inputs(errorMapperInput),
		Exports: plugin.Contracts(
			plugin.ExportAs(func(boundary *errorBoundary) Middleware { return boundary }),
		),
	},
)

func orderErrorMappers(entries []plugin.Entry[ErrorMapper]) ([]plugin.Entry[ErrorMapper], error) {
	slices.SortFunc(entries, func(left, right plugin.Entry[ErrorMapper]) int {
		return plugin.CompareIdentity(left.Identity, right.Identity)
	})
	graph := ordering.New()
	byName := make(map[string]plugin.Entry[ErrorMapper], len(entries))
	byIdentity := make(map[middlewareIdentity]plugin.Entry[ErrorMapper], len(entries))
	for _, entry := range entries {
		identity := entry.Identity.Normalized()
		id := identityOf(identity)
		if _, exists := byIdentity[id]; exists {
			return nil, &DuplicateErrorMapperIdentityError{Identity: identity}
		}
		entry.Identity = identity
		name := identity.String()
		graph.AddNode(name)
		byName[name] = entry
		byIdentity[id] = entry
	}

	resolve := func(ref OrderRef) []plugin.Entry[ErrorMapper] {
		var matches []plugin.Entry[ErrorMapper]
		for _, entry := range entries {
			if orderRefMatches(ref, entry.Identity) {
				matches = append(matches, entry)
			}
		}
		return matches
	}
	for _, entry := range entries {
		declaration := entry.Value.ErrorOrder()
		for _, relation := range []struct {
			refs      []OrderRef
			direction ordering.Direction
		}{
			{declaration.After, ordering.After},
			{declaration.Before, ordering.Before},
		} {
			for _, ref := range relation.refs {
				targets := resolve(ref)
				if len(targets) == 0 {
					if ref.Required() {
						return nil, &MissingErrorMapperOrderTargetError{
							Mapper: entry.Identity, Reference: ref, Direction: relation.direction,
						}
					}
					continue
				}
				for _, target := range targets {
					if relation.direction == ordering.After {
						graph.AddHardEdge(target.Identity.String(), entry.Identity.String())
					} else {
						graph.AddHardEdge(entry.Identity.String(), target.Identity.String())
					}
				}
			}
		}
	}

	names, _, err := graph.Sort()
	if err != nil {
		return nil, fmt.Errorf("xbc: error mapper ordering failed: %w", err)
	}
	result := make([]plugin.Entry[ErrorMapper], len(names))
	for i, name := range names {
		result[i] = byName[name]
	}
	return result, nil
}

// MissingErrorMapperOrderTargetError reports an absent required mapper target.
type MissingErrorMapperOrderTargetError struct {
	Mapper    plugin.Identity
	Reference OrderRef
	Direction ordering.Direction
}

func (e *MissingErrorMapperOrderTargetError) Error() string {
	return fmt.Sprintf(
		"xbc: error mapper %s requires %s=%q, but no matching mapper is present",
		e.Mapper,
		e.Direction,
		e.Reference,
	)
}

// DuplicateErrorMapperIdentityError reports duplicate mapper producer identity.
type DuplicateErrorMapperIdentityError struct{ Identity plugin.Identity }

func (e *DuplicateErrorMapperIdentityError) Error() string {
	return fmt.Sprintf("xbc: error mapper identity %s is present more than once", e.Identity)
}
