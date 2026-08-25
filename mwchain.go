package xbc

import (
	"fmt"
	"slices"

	"github.com/xbcio/xbc/internal/graph"
)

// mwEntry is one middleware after name qualification. qname is the graph
// node id and the identity After/Before reference; plugin is kept for error
// copy that needs to name the owning plugin, not just the middleware.
type mwEntry struct {
	Middleware
	qname  string
	plugin string
}

// PhaseConflictError reports a cross-phase After/Before constraint whose
// direction contradicts Phase order. Phase is a hard boundary (spec §5.8
// rule 1): the only two ways to honor a constraint that fights it are to
// silently reorder the phases (the "wrong onion" the design doc explicitly
// rejects) or to abort. This type carries both middleware names and both
// phase names so the startup log can point at the exact contradiction.
type PhaseConflictError struct {
	From, To           string
	FromPhase, ToPhase Phase
	Dir                string // "after" or "before"
}

func (e *PhaseConflictError) Error() string {
	return fmt.Sprintf(
		"xbc: 中间件 %s（阶段 %s）声明 %s=%q（阶段 %s），与 Phase 先后顺序矛盾，无法排出一致的中间件链",
		e.From, e.FromPhase, e.Dir, e.To, e.ToPhase,
	)
}

// orderMiddlewares groups entries by Phase -- a hard boundary, so the coarse
// position of every entry is fixed before any After/Before is even looked
// at -- sorts each group internally with internal/graph (the same sorter
// stage 4 uses for plugin Init order and Stop order), then concatenates the
// groups in ascending Phase order.
//
// A cross-phase After/Before that names a real, resolvable middleware is
// only ever checked for direction, never turned into a sort edge: adding it
// as an edge would let a single soft constraint silently override the Phase
// boundary. A same-direction cross-phase constraint is redundant (Phase
// order already satisfies it) and is dropped; an opposite-direction one
// aborts with *PhaseConflictError. A reference to a name that was never
// registered at all -- same phase or not -- is reported as a graph.Miss and
// otherwise ignored, matching the soft-edge semantics of internal/graph.
func orderMiddlewares(entries []mwEntry) (ordered []mwEntry, misses []graph.Miss, err error) {
	byName := make(map[string]*mwEntry, len(entries))
	for i := range entries {
		e := &entries[i]
		if _, dup := byName[e.qname]; dup {
			return nil, nil, fmt.Errorf(
				"xbc: 中间件名 %q 重复注册，After/Before 靠名字引用，重名会让引用变成掷骰子", e.qname)
		}
		byName[e.qname] = e
	}

	phaseGraphs := make(map[Phase]*graph.Graph)
	var phaseOrder []Phase
	graphFor := func(ph Phase) *graph.Graph {
		g, ok := phaseGraphs[ph]
		if !ok {
			g = graph.New()
			phaseGraphs[ph] = g
			phaseOrder = append(phaseOrder, ph)
		}
		return g
	}

	// Register every node before any edge, in original registration order,
	// so the stable tiebreak inside internal/graph reflects registration
	// order rather than the order edges happen to be discovered in below.
	for _, e := range entries {
		graphFor(e.Phase).AddNode(e.qname)
	}

	for i := range entries {
		e := &entries[i]
		for _, ref := range e.After {
			other, ok := byName[ref]
			if !ok {
				misses = append(misses, graph.Miss{Node: e.qname, Ref: ref, Dir: "after"})
				continue
			}
			switch {
			case other.Phase == e.Phase:
				// Same group: a real ordering edge for the intra-group sort below.
				graphFor(e.Phase).AddEdge(other.qname, e.qname, false)
			case other.Phase > e.Phase:
				// e wants "other" before it, but other's Phase already
				// places it after e -- direction contradicts Phase order.
				return nil, nil, &PhaseConflictError{
					From: e.qname, To: other.qname,
					FromPhase: e.Phase, ToPhase: other.Phase, Dir: "after",
				}
			}
			// other.Phase < e.Phase: Phase order already puts other first;
			// the constraint is redundant, and is dropped here.
		}
		for _, ref := range e.Before {
			other, ok := byName[ref]
			if !ok {
				misses = append(misses, graph.Miss{Node: e.qname, Ref: ref, Dir: "before"})
				continue
			}
			switch {
			case other.Phase == e.Phase:
				graphFor(e.Phase).AddEdge(e.qname, other.qname, false)
			case other.Phase < e.Phase:
				// e wants to come before "other", but other's Phase
				// already places it before e -- direction contradicts.
				return nil, nil, &PhaseConflictError{
					From: e.qname, To: other.qname,
					FromPhase: e.Phase, ToPhase: other.Phase, Dir: "before",
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
			return nil, nil, fmt.Errorf("xbc: 阶段 %s 内的中间件排序失败: %w", ph, sortErr)
		}
		for _, name := range order {
			ordered = append(ordered, *byName[name])
		}
	}
	return ordered, misses, nil
}
