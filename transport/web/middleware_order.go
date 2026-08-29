package web

import (
	"fmt"
	"slices"

	"github.com/xbcio/xbc/plugin/ordering"
)

// mwEntry is one middleware after Definition-key qualification. qname is both
// the graph node ID and the identity used by After/Before references.
type mwEntry struct {
	Middleware
	qname string
}

// PhaseConflictError reports a cross-phase After/Before constraint whose
// direction contradicts Phase order. Phase is a hard boundary (design §5.8
// rule 1, carried over unchanged from the pre-split kernel): the only two
// ways to honor a constraint that fights it are to silently reorder the
// phases (the "wrong onion" the design doc explicitly rejects) or to abort.
// This type carries both middleware names and both phase names so the
// startup log can point at the exact contradiction.
type PhaseConflictError struct {
	From, To           string
	FromPhase, ToPhase Phase
	Dir                ordering.Direction
}

func (e *PhaseConflictError) Error() string {
	return fmt.Sprintf(
		"xbc: middleware %s (phase %s) declared %s=%q (phase %s), conflicting with phase order, cannot form a consistent middleware chain",
		e.From, e.FromPhase, e.Dir, e.To, e.ToPhase,
	)
}

// orderMiddlewares groups entries by Phase -- a hard boundary, so the coarse
// position of every entry is fixed before any After/Before is even looked
// at -- sorts each group internally with ordering (the same sorter core uses
// for plugin Init order and Stop order), then concatenates the groups in
// ascending Phase order.
//
// A cross-phase After/Before that names a real, resolvable middleware is
// only ever checked for direction, never turned into a sort edge: adding it
// as an edge would let a single soft constraint silently override the Phase
// boundary. A same-direction cross-phase constraint is redundant (Phase
// order already satisfies it) and is dropped; an opposite-direction one
// aborts with *PhaseConflictError. A reference to a name that was never
// registered at all -- same phase or not -- is reported as an ordering.Miss
// and otherwise ignored, matching the soft-edge semantics of ordering.Graph.
func orderMiddlewares(entries []mwEntry) (ordered []mwEntry, misses []ordering.Miss, err error) {
	byName := make(map[string]*mwEntry, len(entries))
	for i := range entries {
		e := &entries[i]
		if _, dup := byName[e.qname]; dup {
			return nil, nil, fmt.Errorf(
				"xbc: middleware name %q is registered more than once; After/Before references require unique names", e.qname)
		}
		byName[e.qname] = e
	}

	phaseGraphs := make(map[Phase]*ordering.Graph)
	var phaseOrder []Phase
	graphFor := func(ph Phase) *ordering.Graph {
		g, ok := phaseGraphs[ph]
		if !ok {
			g = ordering.New()
			phaseGraphs[ph] = g
			phaseOrder = append(phaseOrder, ph)
		}
		return g
	}

	// Register every node before any edge, in original registration order,
	// so the stable tiebreak inside ordering reflects registration order
	// rather than the order edges happen to be discovered in below.
	for _, e := range entries {
		graphFor(e.Phase).AddNode(e.qname)
	}

	for i := range entries {
		e := &entries[i]
		for _, ref := range e.After {
			other, ok := byName[ref]
			if !ok {
				misses = append(misses, ordering.Miss{Node: e.qname, Ref: ref, Dir: ordering.After})
				continue
			}
			switch {
			case other.Phase == e.Phase:
				// Same group: a real ordering edge for the intra-group sort below.
				graphFor(e.Phase).AddSoftEdge(other.qname, e.qname)
			case other.Phase > e.Phase:
				// e wants "other" before it, but other's Phase already
				// places it after e -- direction contradicts Phase order.
				return nil, nil, &PhaseConflictError{
					From: e.qname, To: other.qname,
					FromPhase: e.Phase, ToPhase: other.Phase, Dir: ordering.After,
				}
			}
			// other.Phase < e.Phase: Phase order already puts other first;
			// the constraint is redundant, and is dropped here.
		}
		for _, ref := range e.Before {
			other, ok := byName[ref]
			if !ok {
				misses = append(misses, ordering.Miss{Node: e.qname, Ref: ref, Dir: ordering.Before})
				continue
			}
			switch {
			case other.Phase == e.Phase:
				graphFor(e.Phase).AddSoftEdge(e.qname, other.qname)
			case other.Phase < e.Phase:
				// e wants to come before "other", but other's Phase
				// already places it before e -- direction contradicts.
				return nil, nil, &PhaseConflictError{
					From: e.qname, To: other.qname,
					FromPhase: e.Phase, ToPhase: other.Phase, Dir: ordering.Before,
				}
			}
			// other.Phase > e.Phase: Phase order already puts e first;
			// the constraint is redundant, and is dropped here.
		}
	}

	slices.Sort(phaseOrder)

	ordered = make([]mwEntry, 0, len(entries))
	for _, ph := range phaseOrder {
		// Every node and edge in this graph belongs to entries already
		// known to exist in byName, so Sort's own miss-reporting never
		// fires here; a non-nil err can only be a cycle within this phase.
		order, _, sortErr := phaseGraphs[ph].Sort()
		if sortErr != nil {
			return nil, nil, fmt.Errorf("xbc: middleware ordering failed in phase %s: %w", ph, sortErr)
		}
		for _, name := range order {
			ordered = append(ordered, *byName[name])
		}
	}
	return ordered, misses, nil
}
