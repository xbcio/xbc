package autoload

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/internal/pluginmodel"
	"github.com/xbcio/xbc/plugin"
)

// withPrivateCatalog swaps the process-global catalog's contents for the
// duration of one test and restores them afterwards. The production contract
// is preserved: there is still no exported reset, and a frozen catalog still
// rejects every later declaration. Without this, the first test to freeze
// would permanently bar every later one -- and `go test -count=N` repeats
// tests in one process, so the globals survive between iterations too.
func withPrivateCatalog(t *testing.T) {
	t.Helper()
	defaultCatalog.Lock()
	bundles, frozen, snapshot := defaultCatalog.bundles, defaultCatalog.frozen, defaultCatalog.snapshot
	defaultCatalog.bundles, defaultCatalog.frozen, defaultCatalog.snapshot = nil, false, plugin.Bundle{}
	defaultCatalog.Unlock()

	t.Cleanup(func() {
		defaultCatalog.Lock()
		defaultCatalog.bundles, defaultCatalog.frozen, defaultCatalog.snapshot = bundles, frozen, snapshot
		defaultCatalog.Unlock()
	})
}

type catalogTestValue struct{}

func catalogTestBundle(keys ...plugin.Key) plugin.Bundle {
	definitions := make([]plugin.Definition, len(keys))
	for i, key := range keys {
		definitions[i] = plugin.Define(key, func(plugin.BuildContext) (*catalogTestValue, error) {
			return &catalogTestValue{}, nil
		})
	}
	return plugin.BundleOf(definitions...)
}

func catalogTestKeys(bundle plugin.Bundle) []plugin.Key {
	entries := pluginmodel.BundleEntries(pluginmodel.Bundle(bundle))
	keys := make([]plugin.Key, len(entries))
	for i, entry := range entries {
		descriptor, ok := pluginmodel.DescribeDefinition(entry.Definition)
		if !ok {
			continue
		}
		keys[i] = plugin.Key(descriptor.Key)
	}
	return keys
}

func TestFreezeFlattensEveryDeclarationInOrder(t *testing.T) {
	withPrivateCatalog(t)
	Declare(catalogTestBundle("first", "second"))
	Declare(catalogTestBundle("third"))

	assert.Equal(t, []plugin.Key{"first", "second", "third"}, catalogTestKeys(Freeze()),
		"Freeze flattens every declared Bundle into one composition")
}

func TestFreezeIsIdempotentAndReturnsTheSameSnapshot(t *testing.T) {
	withPrivateCatalog(t)
	Declare(catalogTestBundle("cached"))

	first, second := Freeze(), Freeze()
	assert.Equal(t, catalogTestKeys(first), catalogTestKeys(second))
	assert.Equal(t, 1, len(catalogTestKeys(second)),
		"a second Freeze returns the cached snapshot instead of recombining an already-cleared slice")
}

func TestFreezeOfAnEmptyCatalogIsAUsableEmptyComposition(t *testing.T) {
	withPrivateCatalog(t)
	assert.Empty(t, catalogTestKeys(Freeze()),
		"a process that declared nothing still gets a well-formed Bundle, not a panic")
}

func TestDeclarePanicsAfterFreezeAndNamesTheLateBundles(t *testing.T) {
	withPrivateCatalog(t)
	Declare(catalogTestBundle("in-time"))
	require.NotEmpty(t, catalogTestKeys(Freeze()))

	assert.PanicsWithValue(t,
		"xbc: autoload declaration after default composition was frozen (2 bundles)",
		func() { Declare(catalogTestBundle("too-late"), catalogTestBundle("also-late")) },
		"a Bundle declared after composition closed could never reach the plan, so accepting it would hide the bug")
}

func TestConcurrentDeclareAndFreezeStayConsistent(t *testing.T) {
	withPrivateCatalog(t)

	var declarers sync.WaitGroup
	for i := range 8 {
		declarers.Add(1)
		go func() {
			defer declarers.Done()
			// A Declare losing the race with Freeze panics by contract; this
			// test is about the catalog staying internally consistent, not
			// about which side wins.
			defer func() { _ = recover() }()
			Declare(catalogTestBundle(plugin.Key("racy-" + string(rune('a'+i)))))
		}()
	}
	declarers.Add(1)
	go func() { defer declarers.Done(); Freeze() }()
	declarers.Wait()

	assert.Equal(t, catalogTestKeys(Freeze()), catalogTestKeys(Freeze()),
		"once frozen, the composition never changes again")
}
