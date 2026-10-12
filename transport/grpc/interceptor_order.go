package grpc

import (
	"fmt"
	"slices"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/ordering"
)

// chainKind names the chain an ordering diagnostic is about. Unary and stream
// chains are sorted independently, so a message that did not say which one
// failed would leave an operator to guess.
type chainKind string

const (
	unaryChain  chainKind = "unary"
	streamChain chainKind = "stream"
)

// InterceptorOrderMiss reports one preferred target that contributes no
// interceptor to the chain being sorted. Missing preferences do not weaken any
// present edge; they are omitted only because there is no target to order.
type InterceptorOrderMiss struct {
	Chain       string
	Interceptor plugin.Identity
	Reference   OrderRef
	Direction   ordering.Direction
}

func (m InterceptorOrderMiss) String() string {
	return fmt.Sprintf("%s interceptor %s prefers %s %s", m.Chain, m.Interceptor, m.Direction, m.Reference)
}

// MissingInterceptorOrderTargetError reports an absent required order target.
type MissingInterceptorOrderTargetError struct {
	Chain       string
	Interceptor plugin.Identity
	Reference   OrderRef
	Direction   ordering.Direction
}

func (e *MissingInterceptorOrderTargetError) Error() string {
	return fmt.Sprintf(
		"xbc: %s interceptor %s requires %s=%q, but no matching %s interceptor is present",
		e.Chain, e.Interceptor, e.Direction, e.Reference, e.Chain,
	)
}

// InterceptorPhaseConflictError reports an order constraint whose direction
// contradicts the hard Phase boundary.
type InterceptorPhaseConflictError struct {
	Chain              string
	From, To           plugin.Identity
	FromPhase, ToPhase Phase
	Direction          ordering.Direction
}

func (e *InterceptorPhaseConflictError) Error() string {
	return fmt.Sprintf(
		"xbc: %s interceptor %s (phase %s) declared %s=%q (phase %s), conflicting with phase order, cannot form a consistent interceptor chain",
		e.Chain, e.From, e.FromPhase, e.Direction, e.To, e.ToPhase,
	)
}

// InterceptorPhaseOutsideRecoveryError reports an interceptor placed before
// PhaseRecover, where it would run outside the Server's panic boundary.
type InterceptorPhaseOutsideRecoveryError struct {
	Chain    string
	Identity plugin.Identity
	Phase    Phase
}

func (e *InterceptorPhaseOutsideRecoveryError) Error() string {
	return fmt.Sprintf(
		"xbc: %s interceptor %s declares phase %s, outside the Server's panic boundary at %s; no interceptor may sit where a panic escapes every ordered stage",
		e.Chain, e.Identity, e.Phase, PhaseRecover,
	)
}

// DuplicateInterceptorIdentityError reports two contributions to one chain
// attributed to the same producer. Plugin Identity is the complete interceptor
// identity; a plugin contributes at most one unary and one stream interceptor.
type DuplicateInterceptorIdentityError struct {
	Chain    string
	Identity plugin.Identity
}

func (e *DuplicateInterceptorIdentityError) Error() string {
	return fmt.Sprintf("xbc: %s interceptor identity %s is present more than once", e.Chain, e.Identity)
}

// ordered is what both interceptor contracts share for sorting purposes.
type ordered interface {
	Order() Order
}

// frameworkPins are ordering constraints the Server adds on its own behalf.
type frameworkPins struct {
	outermost []plugin.Identity
	after     []afterPin
}

// afterPin makes predecessor a required predecessor of one exact interceptor.
type afterPin struct {
	interceptor plugin.Identity
	predecessor OrderRef
}

type interceptorNode[T ordered] struct {
	entry plugin.Entry[T]
	order Order
}

// orderInterceptors freezes Order values, groups entries by the hard Phase
// boundary, and topologically sorts each phase. Canonical plugin Identity, not
// collection order, is the tie-break between unconstrained entries -- the same
// rule the Web transport applies to its middleware, implemented here on this
// package's own types.
//
// pins carries the framework's own constraints: stages that must run before
// every other entry of their own phase, and predecessor edges the framework
// adds to a contributed entry (an authentication.RequiresPrincipal
// interceptor after the authentication stage). Prefer and Require differ only
// when a target is absent; every resolved edge is mandatory, so a phase
// contradiction or a same-phase cycle always fails.
func orderInterceptors[T ordered](
	chain chainKind,
	entries []plugin.Entry[T],
	pins frameworkPins,
) (result []plugin.Entry[T], misses []InterceptorOrderMiss, err error) {
	name := string(chain)
	nodes := make([]interceptorNode[T], len(entries))
	seen := make(map[plugin.Identity]bool, len(entries))
	for i, entry := range entries {
		identity := entry.Identity.Normalized()
		if seen[identity] {
			return nil, nil, &DuplicateInterceptorIdentityError{Chain: name, Identity: entry.Identity}
		}
		seen[identity] = true
		order := entry.Value.Order()
		if order.Phase < PhaseRecover {
			return nil, nil, &InterceptorPhaseOutsideRecoveryError{Chain: name, Identity: entry.Identity, Phase: order.Phase}
		}
		nodes[i] = interceptorNode[T]{entry: entry, order: order}
	}

	// ordering.Graph uses node insertion as its stable tie-break, so sorting
	// first makes Identity, rather than collection order, authoritative.
	slices.SortFunc(nodes, func(a, b interceptorNode[T]) int {
		return plugin.CompareIdentity(a.entry.Identity, b.entry.Identity)
	})

	graphs := make(map[Phase]*ordering.Graph)
	byName := make(map[string]*interceptorNode[T], len(nodes))
	phases := make([]Phase, 0)
	graphFor := func(phase Phase) *ordering.Graph {
		if graph, ok := graphs[phase]; ok {
			return graph
		}
		graph := ordering.New()
		graphs[phase] = graph
		phases = append(phases, phase)
		return graph
	}
	for i := range nodes {
		node := &nodes[i]
		key := node.entry.Identity.String()
		graphFor(node.order.Phase).AddNode(key)
		byName[key] = node
	}

	apply := func(from *interceptorNode[T], ref OrderRef, direction ordering.Direction) error {
		var targets []*interceptorNode[T]
		for i := range nodes {
			if ref.matches(nodes[i].entry.Identity) {
				targets = append(targets, &nodes[i])
			}
		}
		if len(targets) == 0 {
			if ref.Required() {
				return &MissingInterceptorOrderTargetError{Chain: name, Interceptor: from.entry.Identity, Reference: ref, Direction: direction}
			}
			misses = append(misses, InterceptorOrderMiss{Chain: name, Interceptor: from.entry.Identity, Reference: ref, Direction: direction})
			return nil
		}
		for _, target := range targets {
			fromPhase, toPhase := from.order.Phase, target.order.Phase
			conflict := (direction == ordering.After && toPhase > fromPhase) ||
				(direction == ordering.Before && toPhase < fromPhase)
			if conflict {
				return &InterceptorPhaseConflictError{
					Chain: name, From: from.entry.Identity, To: target.entry.Identity,
					FromPhase: fromPhase, ToPhase: toPhase, Direction: direction,
				}
			}
			if toPhase != fromPhase {
				continue
			}
			fromKey, toKey := from.entry.Identity.String(), target.entry.Identity.String()
			if direction == ordering.After {
				fromKey, toKey = toKey, fromKey
			}
			graphFor(fromPhase).AddHardEdge(fromKey, toKey)
		}
		return nil
	}

	for i := range nodes {
		node := &nodes[i]
		for _, ref := range node.order.After {
			if err := apply(node, ref, ordering.After); err != nil {
				return nil, nil, err
			}
		}
		for _, ref := range node.order.Before {
			if err := apply(node, ref, ordering.Before); err != nil {
				return nil, nil, err
			}
		}
	}

	for _, pin := range pins.after {
		node, ok := byName[pin.interceptor.String()]
		if !ok {
			return nil, nil, fmt.Errorf("xbc: cannot apply framework ordering pin to absent %s interceptor %s", name, pin.interceptor)
		}
		if err := apply(node, pin.predecessor, ordering.After); err != nil {
			return nil, nil, err
		}
	}

	for _, identity := range pins.outermost {
		target, ok := byName[identity.String()]
		if !ok {
			return nil, nil, fmt.Errorf("xbc: cannot apply framework outermost pin to absent %s interceptor %s", name, identity)
		}
		graph := graphFor(target.order.Phase)
		for i := range nodes {
			other := &nodes[i]
			if other == target || other.order.Phase != target.order.Phase {
				continue
			}
			graph.AddHardEdge(target.entry.Identity.String(), other.entry.Identity.String())
		}
	}

	slices.Sort(phases)
	result = make([]plugin.Entry[T], 0, len(nodes))
	for _, phase := range phases {
		keys, _, sortErr := graphs[phase].Sort()
		if sortErr != nil {
			return nil, nil, fmt.Errorf("xbc: %s interceptor ordering failed in phase %s: %w", name, phase, sortErr)
		}
		for _, key := range keys {
			result = append(result, byName[key].entry)
		}
	}
	return result, misses, nil
}
