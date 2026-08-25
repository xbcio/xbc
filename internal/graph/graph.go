// Package graph implements a generic, stable topological sorter.
//
// This package deliberately does not import, mention, or know about any xbc
// type: nodes and edges are plain strings, nothing more. The same sorter is
// reused for at least three unrelated orderings inside xbc -- plugin
// initialization (Task 9's resolve stage), middleware placement within a
// Phase (Task 11's orderMiddlewares), and Stop() ordering during graceful
// shutdown (the reverse of the same order) -- and none of those callers
// agree on what a "node" even is (a plugin instance id, a qualified
// middleware name, ...). Teaching this package about Dep, Ref, or Plugin
// would tie its correctness to xbc's own evolution for zero benefit: every
// caller already has to turn its own domain objects into id strings before
// it can build a Graph anyway, so the string boundary costs nothing and
// buys a package that compiles and tests on its own, forever.
package graph

import (
	"container/heap"
	"fmt"
	"strings"
)

// edge is one declared ordering constraint, kept exactly as AddEdge received
// it. Validation (does each endpoint exist?) is deferred to Sort, not done
// at AddEdge time, so callers are free to add all their edges before all
// their nodes if that's more convenient -- Sort is the only place that needs
// a complete picture.
type edge struct {
	from, to string
	hard     bool
}

// Graph is a stable topological sorter. Node insertion order is the tiebreak
// among nodes that no edge separates, so the same input always sorts the
// same, run after run.
type Graph struct {
	order []string // node ids, in the order AddNode first saw them
	seen  map[string]bool
	index map[string]int // id -> position in order, for the stable tiebreak
	edges []edge
}

// New creates an empty Graph.
func New() *Graph {
	return &Graph{
		seen:  make(map[string]bool),
		index: make(map[string]int),
	}
}

// AddNode registers a node. It is idempotent: the first call fixes the
// node's insertion index (and therefore its tiebreak priority); later calls
// with the same id are no-ops.
func (g *Graph) AddNode(id string) {
	if g.seen[id] {
		return
	}
	g.seen[id] = true
	g.index[id] = len(g.order)
	g.order = append(g.order, id)
}

// AddEdge declares that from must be ordered before to.
//
// hard=true: both endpoints must exist by the time Sort runs; a missing one
// produces a *MissingNodeError and aborts the sort entirely -- a hard edge
// is a promise about the shape of the graph, and a broken promise is a bug
// in the caller, not something to route around.
//
// hard=false: a missing endpoint is not an error. The edge is silently
// dropped from the ordering and reported back as a Miss, because a soft
// edge is only ever a preference ("if this other thing exists, go near it"),
// and half of that preference not existing is completely unremarkable --
// think an After/Before naming a middleware that was never registered.
func (g *Graph) AddEdge(from, to string, hard bool) {
	g.edges = append(g.edges, edge{from: from, to: to, hard: hard})
}

// Miss is a soft edge whose other endpoint was never registered as a node.
type Miss struct {
	Node string // the node that declared the constraint
	Ref  string // the name it referenced, which does not exist
	Dir  string // "after" or "before"
}

// CycleError reports a dependency cycle with the full path, the starting
// node repeated at the end: {"user", "order", "payment", "user"}.
type CycleError struct{ Path []string }

func (e *CycleError) Error() string {
	return "graph: 存在环 " + strings.Join(e.Path, " → ")
}

// MissingNodeError reports a hard edge pointing at a node that does not
// exist. Both From and To are the edge's own endpoints as declared, even
// though only one of them is necessarily the missing one -- the caller
// already has both values on hand and can tell at a glance which side it
// forgot to AddNode.
type MissingNodeError struct{ From, To string }

func (e *MissingNodeError) Error() string {
	return fmt.Sprintf("graph: 硬依赖引用了不存在的节点：%s → %s", e.From, e.To)
}

// Sort returns nodes in dependency order together with every soft edge that
// referenced a node that does not exist.
//
// Algorithm: Kahn's algorithm (repeatedly emit a node with no remaining
// incoming edges, then decrement its successors' counts), with one twist --
// whenever more than one node is eligible to be emitted at the same time,
// the one that was AddNode'd earliest wins. That tiebreak is implemented as
// a container/heap min-heap keyed by insertion index rather than a plain
// FIFO queue, because eligibility doesn't arrive in insertion order: a node
// inserted first can easily become eligible after one inserted later (it
// might have more incoming edges to clear first), so the pool of
// "currently eligible" nodes needs to be kept sorted by index at all times,
// not just enqueued in the order they become eligible.
func (g *Graph) Sort() ([]string, []Miss, error) {
	adj := make(map[string][]string, len(g.order))
	indeg := make(map[string]int, len(g.order))
	for _, id := range g.order {
		indeg[id] = 0
	}

	var misses []Miss

	for _, e := range g.edges {
		fromOK := g.seen[e.from]
		toOK := g.seen[e.to]

		if fromOK && toOK {
			adj[e.from] = append(adj[e.from], e.to)
			indeg[e.to]++
			continue
		}

		if e.hard {
			return nil, nil, &MissingNodeError{From: e.from, To: e.to}
		}

		if m, ok := missFor(e, fromOK, toOK); ok {
			misses = append(misses, m)
		}
	}

	h := &idHeap{index: g.index}
	for _, id := range g.order {
		if indeg[id] == 0 {
			heap.Push(h, id)
		}
	}

	order := make([]string, 0, len(g.order))
	for h.Len() > 0 {
		id := heap.Pop(h).(string)
		order = append(order, id)
		for _, next := range adj[id] {
			indeg[next]--
			if indeg[next] == 0 {
				heap.Push(h, next)
			}
		}
	}

	if len(order) < len(g.order) {
		remaining := make(map[string]bool, len(g.order)-len(order))
		for _, id := range g.order {
			remaining[id] = true
		}
		for _, id := range order {
			delete(remaining, id)
		}
		return nil, nil, &CycleError{Path: findCycle(g.order, remaining, adj)}
	}

	return order, misses, nil
}

// missFor decides which endpoint of a soft edge with at least one missing
// side "declared the constraint" and which side is the dangling reference.
//
// The convention: whichever endpoint DOES exist is the node that declared
// the constraint (it's the only one that could have -- the other one isn't
// even in the graph to have declared anything), and the direction word
// describes what it said about the missing side. from exists, to missing:
// from said "I go before to", so Dir is "before". to exists, from missing:
// to said "I go after from", so Dir is "after".
func missFor(e edge, fromOK, toOK bool) (Miss, bool) {
	switch {
	case fromOK && !toOK:
		return Miss{Node: e.from, Ref: e.to, Dir: "before"}, true
	case !fromOK && toOK:
		return Miss{Node: e.to, Ref: e.from, Dir: "after"}, true
	default:
		// Neither endpoint exists, so there is no real node to attribute
		// the miss to; an edge between two nodes that both don't exist
		// carries no information worth reporting.
		return Miss{}, false
	}
}

// findCycle runs a DFS over the subgraph induced by remaining (the nodes
// Kahn's algorithm never managed to emit, because they're stuck in a cycle
// or depend on one) and returns the first cycle it finds, as a full path
// with the starting node repeated at the end.
//
// insertionOrder (rather than ranging over the remaining map directly) is
// what makes the result deterministic: map iteration order is randomized by
// Go itself, so picking DFS roots and successors straight from a map would
// make Sort's error message flip between equally-valid cycles from one run
// to the next on the very same graph.
func findCycle(insertionOrder []string, remaining map[string]bool, adj map[string][]string) []string {
	visited := make(map[string]bool, len(remaining))
	onStack := make(map[string]bool, len(remaining))
	var stack []string
	var found []string

	var visit func(id string) bool
	visit = func(id string) bool {
		visited[id] = true
		onStack[id] = true
		stack = append(stack, id)

		for _, next := range adj[id] {
			if !remaining[next] {
				continue
			}
			if onStack[next] {
				start := 0
				for i, s := range stack {
					if s == next {
						start = i
						break
					}
				}
				found = append(append([]string{}, stack[start:]...), next)
				return true
			}
			if !visited[next] {
				if visit(next) {
					return true
				}
			}
		}

		onStack[id] = false
		stack = stack[:len(stack)-1]
		return false
	}

	for _, id := range insertionOrder {
		if !remaining[id] {
			continue
		}
		if !visited[id] {
			if visit(id) {
				return found
			}
		}
	}
	return nil
}

// idHeap is a container/heap min-heap of node ids, ordered by each id's
// insertion index rather than by the id string itself -- Sort's stability
// guarantee is about insertion order, not lexical order.
type idHeap struct {
	ids   []string
	index map[string]int
}

func (h idHeap) Len() int           { return len(h.ids) }
func (h idHeap) Less(i, j int) bool { return h.index[h.ids[i]] < h.index[h.ids[j]] }
func (h idHeap) Swap(i, j int)      { h.ids[i], h.ids[j] = h.ids[j], h.ids[i] }
func (h *idHeap) Push(x any)        { h.ids = append(h.ids, x.(string)) }
func (h *idHeap) Pop() any {
	old := h.ids
	n := len(old)
	item := old[n-1]
	h.ids = old[:n-1]
	return item
}
