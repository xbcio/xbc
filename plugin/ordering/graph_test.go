package ordering

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSortOnEmptyGraph(t *testing.T) {
	g := New()
	order, misses, err := g.Sort()
	require.NoError(t, err)
	assert.Empty(t, order)
	assert.Empty(t, misses)
}

func TestSortLinearChain(t *testing.T) {
	g := New()
	g.AddNode("a")
	g.AddNode("b")
	g.AddNode("c")
	g.AddHardEdge("a", "b")
	g.AddHardEdge("b", "c")

	order, misses, err := g.Sort()
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t, []string{"a", "b", "c"}, order)
}

// TestSortSameLayerStability repeats the same unrelated-nodes graph 100
// times to lock in that Sort's insertion-order tiebreak is not an accident
// of map iteration -- three nodes with no edges between them at all must
// come back in AddNode order, every single time.
func TestSortSameLayerStability(t *testing.T) {
	for i := 0; i < 100; i++ {
		g := New()
		g.AddNode("z")
		g.AddNode("m")
		g.AddNode("a")

		order, misses, err := g.Sort()
		require.NoError(t, err)
		assert.Empty(t, misses)
		assert.Equal(t, []string{"z", "m", "a"}, order,
			"Three unrelated nodes must be strictly ordered by insertion, the %d-th time", i)
	}
}

// TestSortDiamondDependency covers a > b, a > c, b > d, c > d: b and c
// become eligible at the same time once a is emitted, and must come out in
// their own insertion order (b before c) before d, which needs both.
func TestSortDiamondDependency(t *testing.T) {
	g := New()
	g.AddNode("a")
	g.AddNode("b")
	g.AddNode("c")
	g.AddNode("d")
	g.AddHardEdge("a", "b")
	g.AddHardEdge("a", "c")
	g.AddHardEdge("b", "d")
	g.AddHardEdge("c", "d")

	order, misses, err := g.Sort()
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t, []string{"a", "b", "c", "d"}, order)
}

func TestSortDetectsSelfLoop(t *testing.T) {
	g := New()
	g.AddNode("a")
	g.AddHardEdge("a", "a")

	_, _, err := g.Sort()
	require.Error(t, err)
	var cycleErr *CycleError
	require.ErrorAs(t, err, &cycleErr)
	assert.Equal(t, []string{"a", "a"}, cycleErr.Path)
}

func TestSortDetectsTwoNodeMutualCycle(t *testing.T) {
	g := New()
	g.AddNode("a")
	g.AddNode("b")
	g.AddHardEdge("a", "b")
	g.AddHardEdge("b", "a")

	_, _, err := g.Sort()
	require.Error(t, err)
	var cycleErr *CycleError
	require.ErrorAs(t, err, &cycleErr)
	assert.Equal(t, []string{"a", "b", "a"}, cycleErr.Path)
}

// TestSortDetectsThreeNodeCycleWithExactPath locks the exact path and exact
// rendered error text, matching the shape the kernel spec's own error copy
// uses for a plugin dependency cycle (user -> order -> payment -> user).
func TestSortDetectsThreeNodeCycleWithExactPath(t *testing.T) {
	g := New()
	g.AddNode("user")
	g.AddNode("order")
	g.AddNode("payment")
	g.AddHardEdge("user", "order")
	g.AddHardEdge("order", "payment")
	g.AddHardEdge("payment", "user")

	order, misses, err := g.Sort()
	require.Error(t, err)
	assert.Nil(t, order)
	assert.Nil(t, misses)

	var cycleErr *CycleError
	require.ErrorAs(t, err, &cycleErr)
	assert.Equal(t, []string{"user", "order", "payment", "user"}, cycleErr.Path)
	assert.Equal(t, "graph: cycle detected user → order → payment → user", cycleErr.Error())
}

func TestSortReportsSoftMissWithoutError(t *testing.T) {
	g := New()
	g.AddNode("a")
	g.AddSoftEdge("a", "ghost")

	order, misses, err := g.Sort()
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, order)
	require.Len(t, misses, 1)
	assert.Equal(t, Miss{Node: "a", Ref: "ghost", Dir: Before}, misses[0])
}

func TestSortReportsSoftMissFromMissingFromSide(t *testing.T) {
	g := New()
	g.AddNode("b")
	g.AddSoftEdge("ghost", "b")

	order, misses, err := g.Sort()
	require.NoError(t, err)
	assert.Equal(t, []string{"b"}, order)
	require.Len(t, misses, 1)
	assert.Equal(t, Miss{Node: "b", Ref: "ghost", Dir: After}, misses[0])
}

func TestSortReturnsMissingNodeErrorForHardEdge(t *testing.T) {
	g := New()
	g.AddNode("a")
	g.AddHardEdge("a", "ghost")

	order, misses, err := g.Sort()
	assert.Nil(t, order)
	assert.Nil(t, misses)
	require.Error(t, err)

	var missingErr *MissingNodeError
	require.ErrorAs(t, err, &missingErr)
	assert.Equal(t, "a", missingErr.From)
	assert.Equal(t, "ghost", missingErr.To)
	assert.Equal(t, "graph: hard dependency references non-existent node: a → ghost", missingErr.Error())
}

func TestAddNodeIsIdempotent(t *testing.T) {
	g := New()
	g.AddNode("a")
	g.AddNode("b")
	g.AddNode("a") // must not move a's insertion index

	order, misses, err := g.Sort()
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t, []string{"a", "b"}, order,
		"Repeated AddNode cannot change the insertion order of the node")
}

// TestOrderingOnlyDependsOnStdlib is the automated guard for ordering's
// placement contract: as a plugin-owned cross-module leaf package, it must never grow a
// dependency on any other xbc package (which would risk import cycles once
// other packages start depending on it) or on any third-party module.
//
// Uses go list -deps instead of a source grep for the same reason
// TestArchLeafPackagesDependOnStdlibOnly in
// tests/architecture/architecture_test.go does: it walks the real
// compile-time import graph instead of pattern-matching text that might merely
// mention a package name in a comment or string.
func TestOrderingOnlyDependsOnStdlib(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go command is unavailable, skip dependency direction check")
	}

	const pkg = "github.com/xbcio/xbc/plugin/ordering"

	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	require.NoError(t, err, "go list -deps %s failed", pkg)

	for _, line := range strings.Split(string(out), "\n") {
		dep := strings.TrimSpace(line)
		if dep == "" || dep == pkg {
			continue
		}
		assert.False(t, strings.HasPrefix(dep, "github.com/"),
			"plugin/ordering is a stable shared leaf package, must not depend on any other package inside the repository, found: %s", dep)
		assert.False(t, strings.HasPrefix(dep, "golang.org/x/"),
			"plugin/ordering must not introduce third-party dependencies, found: %s", dep)
	}
}
