package web

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin/ordering"
)

// newFixtureEntry builds an mwEntry the way Start would: a raw Middleware
// plus the owning Definition key that qualify needs. The Handler is a bare
// placeholder -- this file never dispatches an HTTP request, it only orders
// entries by name.
func newFixtureEntry(pluginKey, name string, phase Phase, after, before []string) mwEntry {
	return mwEntry{
		Middleware: Middleware{
			Name:    name,
			Phase:   phase,
			After:   after,
			Before:  before,
			Handler: func(*gin.Context) {},
		},
		qname: qualify(pluginKey, name),
	}
}

func qnames(entries []mwEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.qname
	}
	return out
}

func TestPhaseStringKnownAndOutOfRange(t *testing.T) {
	cases := []struct {
		phase Phase
		want  string
	}{
		{PhaseRecover, "recover"},
		{PhaseObserve, "observe"},
		{PhaseSecurity, "security"},
		{PhaseAuth, "auth"},
		{PhaseBusiness, "business"},
		{Phase(150), "phase(150)"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, c.phase.String(), "String form of Phase(%d)", int(c.phase))
	}
}

// spec §4.4's startup log, cors plugin's cors middleware, ratelimit plugin's ratelimit
// Middleware are rendered as bare names without prefixes — these are two real examples of the "no prefix for same name" rule.
func TestQualifySameNameAsPluginIsNotPrefixed(t *testing.T) {
	assert.Equal(t, "cors", qualify("cors", "cors"),
		"spec §4.4 Example: the cors middleware of the cors plugin should not be displayed as cors.cors")
	assert.Equal(t, "ratelimit", qualify("ratelimit", "ratelimit"),
		"spec §4.4 Example: the ratelimit middleware of the ratelimit plugin follows the same rule")
}

// In the spec §4.4 startup log, the jwt plugin's auth middleware is rendered as jwt.auth—this is a real
// example of the "prefix a different name with the plugin key" rule.
func TestQualifyDifferentNameGetsPluginPrefix(t *testing.T) {
	assert.Equal(t, "jwt.auth", qualify("jwt", "auth"),
		"spec §4.4 Example: the auth middleware of the jwt plugin should be displayed as jwt.auth")
}

// The spec gives no real example of a name that already contains a dot, so this synthetic case verifies
// that qualify returns an already qualified name unchanged and never adds another plugin-key prefix.
func TestQualifyDottedNamePassesThroughUnchanged(t *testing.T) {
	assert.Equal(t, "jwt.auth", qualify("audit", "jwt.auth"),
		"A name that already contains a dot indicates that the caller has already qualified it once, and it cannot be wrapped with another plugin key prefix")
}

func TestOrderMiddlewaresGroupsByPhaseAscendingRegardlessOfRegistrationOrder(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("business", "business", PhaseBusiness, nil, nil),
		newFixtureEntry("recover", "recover", PhaseRecover, nil, nil),
		newFixtureEntry("security", "security", PhaseSecurity, nil, nil),
		newFixtureEntry("auth", "auth", PhaseAuth, nil, nil),
		newFixtureEntry("observe", "observe", PhaseObserve, nil, nil),
	}

	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t, []string{"recover", "observe", "security", "auth", "business"}, qnames(ordered),
		"Phase is a hard boundary; the final order must be sorted in ascending Phase order, regardless of registration order")
}

func TestOrderMiddlewaresWithinGroupRegistrationOrderIsStableAcrossManyRuns(t *testing.T) {
	build := func() []mwEntry {
		return []mwEntry{
			newFixtureEntry("c", "c", PhaseSecurity, nil, nil),
			newFixtureEntry("a", "a", PhaseSecurity, nil, nil),
			newFixtureEntry("b", "b", PhaseSecurity, nil, nil),
		}
	}
	want := []string{"c", "a", "b"}
	for i := 0; i < 100; i++ {
		ordered, misses, err := orderMiddlewares(build())
		require.NoError(t, err)
		assert.Empty(t, misses)
		assert.Equal(t, want, qnames(ordered), "The %d-th run: middleware without constraints within the group must be stably ordered in the registration order", i)
	}
}

func TestOrderMiddlewaresWithinGroupAfterConstraintTakesEffect(t *testing.T) {
	// Registration order is ratelimit first, cors later; if After has no effect, stable sorting will maintain this
	// Registration order. Once ratelimit declares After=cors, the result must reverse.
	entries := []mwEntry{
		newFixtureEntry("ratelimit", "ratelimit", PhaseSecurity, []string{"cors"}, nil),
		newFixtureEntry("cors", "cors", PhaseSecurity, nil, nil),
	}
	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t, []string{"cors", "ratelimit"}, qnames(ordered),
		"ratelimit declares After=cors, even if registration order is ratelimit first, the sorted result must place cors first")
}

func TestOrderMiddlewaresWithinGroupBeforeConstraintTakesEffect(t *testing.T) {
	// Registration order is ratelimit first, cors later; cors declares Before=ratelimit, the result must move
	// cors to before ratelimit, opposite to registration order.
	entries := []mwEntry{
		newFixtureEntry("ratelimit", "ratelimit", PhaseSecurity, nil, nil),
		newFixtureEntry("cors", "cors", PhaseSecurity, nil, []string{"ratelimit"}),
	}
	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err)
	assert.Empty(t, misses)
	assert.Equal(t, []string{"cors", "ratelimit"}, qnames(ordered),
		"cors declares Before=ratelimit, even if registration order is ratelimit first, the sorted result must place cors first")
}

// spec §5.8's audit example: audit (PhaseBusiness) declares After: "jwt.auth"
// (PhaseAuth). 300 < 400, jwt.auth is already before audit, this cross-phase constraint is consistent with
// Phase order, it is a redundant declaration and must be silently ignored—neither error nor miss.
func TestOrderMiddlewaresCrossPhaseConsistentConstraintIsRedundantAndIgnored(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("audit", "audit", PhaseBusiness, []string{"jwt.auth"}, nil),
		newFixtureEntry("jwt", "auth", PhaseAuth, nil, nil),
	}
	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err, "audit(business) After jwt.auth(auth) aligns with Phase order, it is a redundant constraint and should not abort startup")
	assert.Empty(t, misses)
	assert.Equal(t, []string{"jwt.auth", "audit"}, qnames(ordered))
}

// The reverse scenario: a PhaseSecurity middleware declares After: "jwt.auth" (PhaseAuth).
// security(200) is originally before auth(300), but After requires jwt.auth to come before it—
// the direction conflicts with Phase order, startup must be aborted, and the error must include both middleware names and their Phase names.
func TestOrderMiddlewaresCrossPhaseReversedConstraintAbortsWithPhaseConflictError(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("early", "check", PhaseSecurity, []string{"jwt.auth"}, nil),
		newFixtureEntry("jwt", "auth", PhaseAuth, nil, nil),
	}
	_, _, err := orderMiddlewares(entries)
	require.Error(t, err, "early.check(security) After jwt.auth(auth) contradicts Phase order, startup must be aborted")

	var conflict *PhaseConflictError
	require.ErrorAs(t, err, &conflict, "Conflict must be convertible into *PhaseConflictError, allowing upper layers to distinguish from other failure causes")
	assert.Equal(t, "early.check", conflict.From)
	assert.Equal(t, "jwt.auth", conflict.To)
	assert.Equal(t, PhaseSecurity, conflict.FromPhase)
	assert.Equal(t, PhaseAuth, conflict.ToPhase)
	assert.Contains(t, err.Error(), "early.check", "Error message must include the middleware name declaring the constraint")
	assert.Contains(t, err.Error(), "jwt.auth", "Error message must include the referenced middleware name")
	assert.Contains(t, err.Error(), "security", "Error message must include the declared Phase name")
	assert.Contains(t, err.Error(), "auth", "Error message must include the referenced Phase name")
}

// The original test covered only the After branch's consistency check. The Before branch (the second
// loop in orderMiddlewares) has separate comparison logic and needs its own fixture: a PhaseSecurity middleware
// declares Before a PhaseAuth middleware. security(200) already precedes auth(300), matching
// Phase order, so the redundant constraint must be ignored through the other half of the code.
func TestOrderMiddlewaresCrossPhaseConsistentBeforeConstraintIsRedundantAndIgnored(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("security", "check", PhaseSecurity, nil, []string{"jwt.auth"}),
		newFixtureEntry("jwt", "auth", PhaseAuth, nil, nil),
	}
	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err, "security.check(security) Before jwt.auth(auth) aligns with Phase order, is a redundant constraint, and should not abort startup")
	assert.Empty(t, misses)
	assert.Equal(t, []string{"security.check", "jwt.auth"}, qnames(ordered))
}

// Reverse case: a PhaseAuth middleware declares Before a PhaseSecurity middleware.
// auth(300) naturally follows security(200), so moving it ahead would conflict with Phase
// order and must abort startup, mirroring the reverse After case through the Before branch.
func TestOrderMiddlewaresCrossPhaseReversedBeforeConstraintAbortsWithPhaseConflictError(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("jwt", "audit", PhaseAuth, nil, []string{"security.gate"}),
		newFixtureEntry("security", "gate", PhaseSecurity, nil, nil),
	}
	_, _, err := orderMiddlewares(entries)
	require.Error(t, err, "jwt.audit(auth) Before security.gate(security) contradicts Phase order and must abort startup")

	var conflict *PhaseConflictError
	require.ErrorAs(t, err, &conflict, "Conflict must be recoverable as *PhaseConflictError")
	assert.Equal(t, "jwt.audit", conflict.From)
	assert.Equal(t, "security.gate", conflict.To)
	assert.Equal(t, PhaseAuth, conflict.FromPhase)
	assert.Equal(t, PhaseSecurity, conflict.ToPhase)
	assert.Equal(t, ordering.Before, conflict.Dir)
	assert.Contains(t, err.Error(), "jwt.audit")
	assert.Contains(t, err.Error(), "security.gate")
	assert.Contains(t, err.Error(), "auth")
	assert.Contains(t, err.Error(), "security")
}

// Guard against merging Phase order and After/Before into one graph that checks boundaries only between
// adjacent Phase groups. Each Phase has two entries here, and the reverse constraint targets the second
// (interior) node auth.a2 of the later group rather than its first node. A boundary-only implementation
// might miss the conflict and silently move auth.a2 before sec.s2. The correct Phase-grouped
// implementation checks every cross-phase reference by Phase value, regardless of position within
// the group, so startup must still abort.
func TestOrderMiddlewaresCrossPhaseReversedConstraintOnInteriorNodeAbortsEvenWithMultipleEntriesPerPhase(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("sec", "s1", PhaseSecurity, nil, nil),
		newFixtureEntry("sec", "s2", PhaseSecurity, []string{"auth.a2"}, nil),
		newFixtureEntry("auth", "a1", PhaseAuth, nil, nil),
		newFixtureEntry("auth", "a2", PhaseAuth, nil, nil),
	}
	_, _, err := orderMiddlewares(entries)
	require.Error(t, err, "sec.s2(security) After auth.a2(auth) attempts to pull the middleware of a later Phase before the previous Phase, even if the target is not the first node in the group, it must still stop startup")

	var conflict *PhaseConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, "sec.s2", conflict.From)
	assert.Equal(t, "auth.a2", conflict.To)
	assert.Equal(t, PhaseSecurity, conflict.FromPhase)
	assert.Equal(t, PhaseAuth, conflict.ToPhase)
}

func TestOrderMiddlewaresMissingReferenceIsRecordedAsMiss(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("audit", "audit", PhaseBusiness, []string{"tracing"}, nil),
	}
	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err, "Reference to a non-existent name is a soft constraint miss, startup should not be stopped")
	require.Len(t, misses, 1)
	assert.Equal(t, ordering.Miss{Node: "audit", Ref: "tracing", Dir: ordering.After}, misses[0])
	assert.Equal(t, []string{"audit"}, qnames(ordered))
}

// Mirror the above After missing reference fixture, verifying that the Before branch is also recorded as miss—this file
// originally had no use cases covering Before's dangling references.
func TestOrderMiddlewaresMissingBeforeReferenceIsRecordedAsMiss(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("audit", "audit", PhaseBusiness, nil, []string{"tracing"}),
	}
	ordered, misses, err := orderMiddlewares(entries)
	require.NoError(t, err, "A missing name is a soft constraint miss, should not abort startup")
	require.Len(t, misses, 1)
	assert.Equal(t, ordering.Miss{Node: "audit", Ref: "tracing", Dir: ordering.Before}, misses[0])
	assert.Equal(t, []string{"audit"}, qnames(ordered))
}

func TestOrderMiddlewaresDuplicateQualifiedNameFails(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("cors", "cors", PhaseSecurity, nil, nil),
		newFixtureEntry("cors", "cors", PhaseSecurity, nil, nil),
	}
	_, _, err := orderMiddlewares(entries)
	require.Error(t, err, "Name collision after qualification, referencing After/Before by name loses uniqueness, must error")
	assert.Contains(t, err.Error(), "cors")
}

func TestOrderMiddlewaresCycleWithinGroupFails(t *testing.T) {
	entries := []mwEntry{
		newFixtureEntry("a", "a", PhaseSecurity, []string{"b"}, nil),
		newFixtureEntry("b", "b", PhaseSecurity, []string{"a"}, nil),
	}
	_, _, err := orderMiddlewares(entries)
	require.Error(t, err, "a After b and b After a, cycle appears within group, must error")

	var cycleErr *ordering.CycleError
	assert.ErrorAs(t, err, &cycleErr, "Cyclic group should be recoverable as ordering.CycleError")
}
