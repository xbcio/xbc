package web

import (
	"fmt"
	"slices"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/ordering"
)

// MiddlewareOrderMiss reports one preferred target that is not present in the
// frozen middleware set. Missing preferences do not weaken any present edge;
// they are omitted only because there is no target to order.
type MiddlewareOrderMiss struct {
	Middleware plugin.Identity
	Reference  OrderRef
	Direction  ordering.Direction
}

func (m MiddlewareOrderMiss) String() string {
	return fmt.Sprintf("middleware %s prefers %s %s", m.Middleware, m.Direction, m.Reference)
}

// MissingMiddlewareOrderTargetError reports an absent required order target.
type MissingMiddlewareOrderTargetError struct {
	Middleware plugin.Identity
	Reference  OrderRef
	Direction  ordering.Direction
}

func (e *MissingMiddlewareOrderTargetError) Error() string {
	return fmt.Sprintf(
		"xbc: middleware %s requires %s=%q, but no matching middleware is present",
		e.Middleware, e.Direction, e.Reference,
	)
}

// PhaseConflictError reports an order constraint whose direction contradicts
// the hard Phase boundary.
type PhaseConflictError struct {
	From, To           plugin.Identity
	FromPhase, ToPhase Phase
	Direction          ordering.Direction
}

func (e *PhaseConflictError) Error() string {
	return fmt.Sprintf(
		"xbc: middleware %s (phase %s) declared %s=%q (phase %s), conflicting with phase order, cannot form a consistent middleware chain",
		e.From, e.FromPhase, e.Direction, e.To, e.ToPhase,
	)
}

// DuplicateMiddlewareIdentityError reports two contributions attributed to the
// same producer. Plugin Identity is the complete middleware identity, so there
// is no name field available to disambiguate this condition.
type DuplicateMiddlewareIdentityError struct {
	Identity plugin.Identity
}

func (e *DuplicateMiddlewareIdentityError) Error() string {
	return fmt.Sprintf("xbc: middleware identity %s is present more than once", e.Identity)
}

type middlewareOrderOptions struct {
	outermost []plugin.Identity
	after     []middlewareAfterPin
}

type middlewareAfterPin struct {
	middleware  plugin.Identity
	predecessor OrderRef
}

// middlewareOrderOption adds framework-owned edges without exposing a second
// public ordering mechanism.
type middlewareOrderOption func(*middlewareOrderOptions)

// pinMiddlewareOutermost pins one exact middleware identity before every other
// entry in its phase. Web uses it for the two stages the Server assembles
// itself -- the error boundary and the authentication middleware -- so the
// target is a framework-owned Identity rather than a contributed key that might
// resolve to several instances or to nothing.
func pinMiddlewareOutermost(identity plugin.Identity) middlewareOrderOption {
	return func(options *middlewareOrderOptions) {
		options.outermost = append(options.outermost, identity)
	}
}

// pinMiddlewareAfter adds a framework-owned predecessor edge for one exact
// middleware identity. Web can use this after detecting a RequiresPrincipal
// marker, without this package importing an authentication implementation. The
// predecessor is made required even if ref was created with Prefer.
func pinMiddlewareAfter(identity plugin.Identity, ref OrderRef) middlewareOrderOption {
	return func(options *middlewareOrderOptions) {
		options.after = append(options.after, middlewareAfterPin{
			middleware:  identity,
			predecessor: ref.asRequired(),
		})
	}
}

type middlewareNode struct {
	entry plugin.Entry[Middleware]
	order Order
	id    middlewareIdentity
}

type middlewareIdentity struct {
	key      plugin.Key
	instance string
}

func identityOf(identity plugin.Identity) middlewareIdentity {
	return middlewareIdentity{
		key:      identity.Plugin,
		instance: plugin.NormalizeInstance(identity.Instance),
	}
}

func compareMiddlewareIdentity(left, right plugin.Identity) int {
	return plugin.CompareIdentity(left, right)
}

func orderRefMatches(ref OrderRef, identity plugin.Identity) bool {
	if identity.Plugin != ref.Key() {
		return false
	}
	return ref.InstanceName() == "" ||
		plugin.NormalizeInstance(identity.Instance) == ref.InstanceName()
}

// orderMiddlewares freezes middleware Order values, groups entries by the hard
// Phase boundary, and topologically sorts each phase. Canonical plugin Identity,
// not registration order, is the tie-break between unconstrained entries.
//
// Prefer and Require differ only when a target is absent. Every resolved edge
// is mandatory: a phase contradiction or same-phase cycle always fails.
func orderMiddlewares(
	entries []plugin.Entry[Middleware],
	optionFns ...middlewareOrderOption,
) (ordered []plugin.Entry[Middleware], misses []MiddlewareOrderMiss, err error) {
	options := middlewareOrderOptions{}
	for _, apply := range optionFns {
		apply(&options)
	}

	nodes := make([]middlewareNode, len(entries))
	byIdentity := make(map[middlewareIdentity]*middlewareNode, len(entries))
	for i, entry := range entries {
		id := identityOf(entry.Identity)
		if _, duplicate := byIdentity[id]; duplicate {
			return nil, nil, &DuplicateMiddlewareIdentityError{Identity: entry.Identity}
		}
		nodes[i] = middlewareNode{
			entry: entry,
			order: entry.Value.Order(),
			id:    id,
		}
		byIdentity[id] = &nodes[i]
	}

	// Canonicalize before adding graph nodes. ordering.Graph uses node insertion
	// as its stable tie-break, so this makes Identity, rather than collection or
	// registration order, authoritative.
	slices.SortFunc(nodes, func(a, b middlewareNode) int {
		return compareMiddlewareIdentity(a.entry.Identity, b.entry.Identity)
	})

	// Rebuild after sorting: pointers into a slice cannot be retained while its
	// elements are being permuted.
	clear(byIdentity)
	for i := range nodes {
		byIdentity[nodes[i].id] = &nodes[i]
	}

	phaseGraphs := make(map[Phase]*ordering.Graph)
	phaseNodes := make(map[Phase]map[string]*middlewareNode)
	phaseOrder := make([]Phase, 0)
	graphFor := func(phase Phase) *ordering.Graph {
		if graph, ok := phaseGraphs[phase]; ok {
			return graph
		}
		graph := ordering.New()
		phaseGraphs[phase] = graph
		phaseNodes[phase] = make(map[string]*middlewareNode)
		phaseOrder = append(phaseOrder, phase)
		return graph
	}

	for i := range nodes {
		node := &nodes[i]
		name := node.entry.Identity.String()
		graphFor(node.order.Phase).AddNode(name)
		phaseNodes[node.order.Phase][name] = node
	}

	resolve := func(ref OrderRef) []*middlewareNode {
		matches := make([]*middlewareNode, 0)
		for i := range nodes {
			if orderRefMatches(ref, nodes[i].entry.Identity) {
				matches = append(matches, &nodes[i])
			}
		}
		return matches
	}

	applyReference := func(
		from *middlewareNode,
		ref OrderRef,
		direction ordering.Direction,
	) error {
		targets := resolve(ref)
		if len(targets) == 0 {
			if ref.Required() {
				return &MissingMiddlewareOrderTargetError{
					Middleware: from.entry.Identity,
					Reference:  ref,
					Direction:  direction,
				}
			}
			misses = append(misses, MiddlewareOrderMiss{
				Middleware: from.entry.Identity,
				Reference:  ref,
				Direction:  direction,
			})
			return nil
		}

		for _, target := range targets {
			fromPhase := from.order.Phase
			toPhase := target.order.Phase
			switch direction {
			case ordering.After:
				switch {
				case toPhase > fromPhase:
					return &PhaseConflictError{
						From: from.entry.Identity, To: target.entry.Identity,
						FromPhase: fromPhase, ToPhase: toPhase,
						Direction: ordering.After,
					}
				case toPhase == fromPhase:
					graphFor(fromPhase).AddHardEdge(
						target.entry.Identity.String(), from.entry.Identity.String())
				}
			case ordering.Before:
				switch {
				case toPhase < fromPhase:
					return &PhaseConflictError{
						From: from.entry.Identity, To: target.entry.Identity,
						FromPhase: fromPhase, ToPhase: toPhase,
						Direction: ordering.Before,
					}
				case toPhase == fromPhase:
					graphFor(fromPhase).AddHardEdge(
						from.entry.Identity.String(), target.entry.Identity.String())
				}
			default:
				return fmt.Errorf("xbc: unknown middleware order direction %d", direction)
			}
		}
		return nil
	}

	for i := range nodes {
		node := &nodes[i]
		for _, ref := range node.order.After {
			if err := applyReference(node, ref, ordering.After); err != nil {
				return nil, nil, err
			}
		}
		for _, ref := range node.order.Before {
			if err := applyReference(node, ref, ordering.Before); err != nil {
				return nil, nil, err
			}
		}
	}

	for _, pin := range options.after {
		node, ok := byIdentity[identityOf(pin.middleware)]
		if !ok {
			return nil, nil, fmt.Errorf(
				"xbc: cannot apply framework order pin to absent middleware %s", pin.middleware)
		}
		if err := applyReference(node, pin.predecessor, ordering.After); err != nil {
			return nil, nil, err
		}
	}

	for _, identity := range options.outermost {
		target, ok := byIdentity[identityOf(identity)]
		if !ok {
			return nil, nil, fmt.Errorf(
				"xbc: cannot apply framework outermost pin to absent middleware %s", identity)
		}
		graph := graphFor(target.order.Phase)
		for i := range nodes {
			other := &nodes[i]
			if other.order.Phase != target.order.Phase || other.id == target.id {
				continue
			}
			graph.AddHardEdge(
				target.entry.Identity.String(), other.entry.Identity.String())
		}
	}

	slices.Sort(phaseOrder)
	ordered = make([]plugin.Entry[Middleware], 0, len(nodes))
	for _, phase := range phaseOrder {
		names, _, sortErr := phaseGraphs[phase].Sort()
		if sortErr != nil {
			return nil, nil, fmt.Errorf(
				"xbc: middleware ordering failed in phase %s: %w", phase, sortErr)
		}
		for _, name := range names {
			ordered = append(ordered, phaseNodes[phase][name].entry)
		}
	}
	return ordered, misses, nil
}
