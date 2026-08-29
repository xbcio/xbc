package catalog

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

func defKeys(defs []plugin.Definition) []string {
	keys := make([]string, len(defs))
	for i, d := range defs {
		keys[i] = d.Key.String()
	}
	return keys
}

type fixturePlugin struct{}

func factoryFor(_ string) plugin.Factory {
	return func() plugin.Plugin { return &fixturePlugin{} }
}

func def(key string) plugin.Definition {
	return plugin.Definition{Key: plugin.Key(key), Factory: factoryFor(key), Activation: plugin.Always}
}

// TestFreezeIsIdempotent pins that calling Freeze twice on the same Catalog
// returns the exact same Snapshot and the exact same error both times --
// not "an equivalent one recomputed", the same one, cached.
func TestFreezeIsIdempotent(t *testing.T) {
	c := New()
	c.Declare(def("gorm"))
	c.Declare(def("redis"))

	snap1, err1 := c.Freeze()
	require.NoError(t, err1)
	snap2, err2 := c.Freeze()
	require.NoError(t, err2)

	// assert.Equal on []plugin.Definition would run reflect.DeepEqual over
	// the Factory func fields, which Go can never consider equal (funcs
	// are only DeepEqual when both nil) -- so this compares what actually
	// carries the "same result" claim: Definition keys, in order, plus the object
	// identity of the two Snapshots' backing definition slices, which
	// proves the second Freeze call returned the cached Snapshot rather
	// than a freshly recomputed one that merely looks the same.
	assert.Equal(t, defKeys(snap1.Definitions()), defKeys(snap2.Definitions()))
	assert.Equal(t, 2, snap2.Len())
	assert.Equal(t, reflect.ValueOf(snap1.defs).Pointer(), reflect.ValueOf(snap2.defs).Pointer(),
		"two Freeze calls must return the same cached Snapshot, not a newly calculated value with the same content")
}

// TestFreezeIsIdempotentAndCachesTheError pins the harder half of
// idempotency: when the first Freeze fails, a second call must return that
// same error again -- not silently succeed, and not re-run validation to
// produce a second, textually-different error.
func TestFreezeIsIdempotentAndCachesTheError(t *testing.T) {
	c := New()
	c.Declare(def("gorm"))
	c.Declare(def("gorm"))

	_, err1 := c.Freeze()
	require.Error(t, err1)
	_, err2 := c.Freeze()
	require.Error(t, err2)
	assert.Equal(t, err1.Error(), err2.Error(), "second call cannot swallow errors, nor report a different error")
}

// TestDeclareOrderIsIrrelevantToFrozenResult pins §3.3 rule 3: two Catalogs
// fed the same definitions in opposite Declare order must produce
// byte-for-byte the same Definitions() slice -- the result must not depend
// on Go's init() ordering, which Declare order stands in for here.
func TestDeclareOrderIsIrrelevantToFrozenResult(t *testing.T) {
	forward := New()
	forward.Declare(def("gorm"))
	forward.Declare(def("redis"))
	forward.Declare(def("jwt"))

	reverse := New()
	reverse.Declare(def("jwt"))
	reverse.Declare(def("redis"))
	reverse.Declare(def("gorm"))

	snapForward, err := forward.Freeze()
	require.NoError(t, err)
	snapReverse, err := reverse.Freeze()
	require.NoError(t, err)

	defsForward := snapForward.Definitions()
	defsReverse := snapReverse.Definitions()
	require.Len(t, defsReverse, len(defsForward))
	for i := range defsForward {
		assert.Equal(t, defsForward[i].Key, defsReverse[i].Key,
			"reversed declaration order, the key order after freeze must remain consistent")
	}
}

// TestDeclarePanicsAfterFreeze pins that a Catalog rejects any further
// Declare call once it has been frozen, regardless of whether Freeze
// succeeded or failed.
func TestDeclarePanicsAfterFreeze(t *testing.T) {
	c := New()
	c.Declare(def("gorm"))
	_, err := c.Freeze()
	require.NoError(t, err)

	assert.PanicsWithValue(t, "xbc: plugin directory is frozen, no more plugins can be declared after init()", func() {
		c.Declare(def("redis"))
	})
}

// TestFreezeReportsDuplicateKeyWithBothSources pins that a key collision
// is reported with enough detail about BOTH conflicting definitions to let
// a reader tell them apart -- not just "gorm was declared twice" with no
// way to know which two call sites collided.
func TestFreezeReportsDuplicateKeyWithBothSources(t *testing.T) {
	c := New()
	c.Declare(plugin.Definition{Key: "gorm", Factory: func() plugin.Plugin { return &fixturePlugin{} }})
	c.Declare(plugin.Definition{Key: "gorm", Factory: func() plugin.Plugin { return &fixturePlugin{} }})

	_, err := c.Freeze()
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "xbc: ")
	assert.Contains(t, msg, `"gorm"`)
	// Both conflicting factories were declared on different lines directly
	// above, so their file:line descriptions necessarily differ -- the
	// error must quote both, not just the second (winning, or losing)
	// declaration.
	assert.Contains(t, msg, "catalog_test.go:")
	firstIdx := strings.Index(msg, "catalog_test.go:")
	require.NotEqual(t, -1, firstIdx)
	rest := msg[firstIdx+len("catalog_test.go:"):]
	secondRelIdx := strings.Index(rest, "catalog_test.go:")
	require.NotEqual(t, -1, secondRelIdx, "error message must include information from both conflicting sources, cannot report only one")

	firstLine := strings.SplitN(msg[firstIdx:], ")", 2)[0]
	secondIdx := firstIdx + len("catalog_test.go:") + secondRelIdx
	secondLine := strings.SplitN(msg[secondIdx:], ")", 2)[0]
	assert.NotEqual(t, firstLine, secondLine, "two sources' file: line numbers must differ, otherwise the reader cannot distinguish between the conflicting parties")
}

// TestSnapshotDefinitionsIsDefensiveCopy pins that mutating the slice
// returned by Definitions() has no effect on a later call.
func TestSnapshotDefinitionsIsDefensiveCopy(t *testing.T) {
	c := New()
	c.Declare(def("gorm"))
	c.Declare(def("redis"))
	snap, err := c.Freeze()
	require.NoError(t, err)

	first := snap.Definitions()
	first[0].Key = "mutated"

	second := snap.Definitions()
	assert.Equal(t, "gorm", second[0].Key.String(), "modifying the slice returned by the previous call must not affect the result of the next call")
}

// TestSnapshotZeroValueIsUsable pins that a bare Snapshot{} (never built by
// Freeze) behaves like an empty, well-formed snapshot rather than panicking.
func TestSnapshotZeroValueIsUsable(t *testing.T) {
	var s Snapshot
	assert.Equal(t, 0, s.Len())
	assert.Empty(t, s.Definitions())
	_, ok := s.Lookup("anything")
	assert.False(t, ok)
}

func TestSnapshotLookupFindsDeclaredDefinition(t *testing.T) {
	c := New()
	c.Declare(def("gorm"))
	c.Declare(def("redis"))
	snap, err := c.Freeze()
	require.NoError(t, err)

	d, ok := snap.Lookup("redis")
	require.True(t, ok)
	assert.Equal(t, "redis", d.Key.String())

	_, ok = snap.Lookup("missing")
	assert.False(t, ok)
}

// TestFreezeRejectsInvalidDefinition pins that Freeze runs
// Definition.Validate on every declared definition, not just the
// duplicate-key check.
func TestFreezeRejectsInvalidDefinition(t *testing.T) {
	c := New()
	c.Declare(plugin.Definition{Key: "", Factory: factoryFor("x")})
	_, err := c.Freeze()
	assert.Error(t, err)
}

func TestFreezeRejectsNilFactory(t *testing.T) {
	c := New()
	c.Declare(plugin.Definition{Key: "gorm", Factory: nil})
	_, err := c.Freeze()
	assert.Error(t, err)
}

// ---- process-wide default catalog ----
//
// Only one test touches the package-level Declare/Freeze: the default
// catalog is a process-wide singleton with a sync.Once-guarded Freeze, so
// once any test freezes it, every other test in this process is
// permanently barred from Declare-ing into it again. Every other test in
// this file uses its own private New() Catalog instead, which is exactly
// the isolation §3.3.1 calls out as the point of Snapshot-based testing.
func TestDefaultCatalogDeclareAndFreeze(t *testing.T) {
	// `go test -count=N` repeats tests in one process, so package globals survive
	// between iterations. Swap in a private singleton for this test and restore
	// the real process-wide catalog afterward. Keeping this fixture in _test.go
	// preserves the production contract: there is still no public Reset, and a
	// frozen production Catalog still rejects every later declaration.
	original := defaultCatalog
	defaultCatalog = New()
	t.Cleanup(func() { defaultCatalog = original })

	Declare(plugin.Definition{Key: "default-catalog-fixture", Factory: factoryFor("default-catalog-fixture")})

	snap, err := Freeze()
	require.NoError(t, err)
	_, ok := snap.Lookup("default-catalog-fixture")
	assert.True(t, ok, "package-level Declare must write to the package-level default directory, and be discoverable after Freeze")

	// Freeze is idempotent for the default catalog too.
	snap2, err2 := Freeze()
	require.NoError(t, err2)
	assert.Equal(t, defKeys(snap.Definitions()), defKeys(snap2.Definitions()))

	assert.PanicsWithValue(t, "xbc: plugin directory is frozen, no more plugins can be declared after init()", func() {
		Declare(def("too-late"))
	})
}
