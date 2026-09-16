package web

import (
	"fmt"
	"slices"

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

// errorBoundary is the outermost PhaseError middleware. The Server assembles it
// rather than selecting it as a plugin, for the same reason it assembles the
// authentication middleware: a framework-owned ordering pin makes it required,
// and a required stage must not be something a composition root can leave out
// or an operator can switch off. As a Definition it was disableable, and
// plugins.web-error-boundary.enabled=false failed startup by tripping that pin
// -- reporting an unsatisfiable internal pin instead of the rule that every
// contributed ErrorMapper needs a boundary to run in. Its identity is reserved
// accordingly; see reserved_middleware.go.
type errorBoundary struct {
	mappers []ErrorMapper
}

func (b *errorBoundary) Handler() Handler { return OnError(b.mappers...) }
func (*errorBoundary) Order() Order       { return Order{Phase: PhaseError} }

// errorBoundaryIdentity is the producer identity the Server attributes its
// built-in error boundary to. Like authenticationIdentity it is an ordering
// anchor rather than a selectable plugin, so Require(ErrorBoundaryKey) resolves
// against a real entry.
var errorBoundaryIdentity = plugin.Identity{Plugin: ErrorBoundaryKey}

// newErrorBoundary builds the Server-owned boundary from every contributed
// ErrorMapper, sorted by the independent ErrorOrder graph. A precedence
// mistake among mappers therefore fails Start, in the same place a middleware
// ordering mistake does.
func newErrorBoundary(entries []plugin.Entry[ErrorMapper]) (*errorBoundary, error) {
	ordered, err := orderErrorMappers(entries)
	if err != nil {
		return nil, err
	}
	mappers := make([]ErrorMapper, len(ordered))
	for i, entry := range ordered {
		mappers[i] = entry.Value
	}
	return &errorBoundary{mappers: mappers}, nil
}

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
