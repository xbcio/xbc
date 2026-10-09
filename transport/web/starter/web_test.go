package starter_test

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	pluginmodel "github.com/xbcio/xbc/plugin/model"
	"github.com/xbcio/xbc/transport/web/starter"
)

// dormantMemberPaths returns the activation path of every Definition in the
// starter's Bundles that stays dormant until a configuration section exists,
// derived from the Definitions themselves. Deriving it rather than listing it
// is the point: a prelude member added later with the same activation policy
// is picked up here without anyone remembering to edit this file.
func dormantMemberPaths(t *testing.T) []string {
	t.Helper()

	var paths []string
	for _, bundle := range starter.Web().Bundles() {
		for _, entry := range pluginmodel.BundleEntries(pluginmodel.Bundle(bundle)) {
			descriptor, ok := pluginmodel.DescribeDefinition(entry.Definition)
			require.True(t, ok, "a Bundle returned by the starter must hold canonical Definitions")
			if descriptor.Activation.Kind != pluginmodel.ActivationConfigured {
				continue
			}
			require.NotEmpty(t, descriptor.Activation.Path,
				"a configured activation without a path could not be satisfied by any configuration")
			paths = append(paths, descriptor.Activation.Path)
		}
	}
	sort.Strings(paths)
	return paths
}

// enabledPaths returns the section each entry of the defaults layer switches
// on, rejecting a spelling the configuration layer would not read back.
func enabledPaths(t *testing.T) []string {
	t.Helper()

	var paths []string
	for path, value := range starter.Web().Defaults().Values {
		require.Equal(t, true, value,
			"the baseline switches a capability on; it does not restate the capability's own values")
		require.Regexp(t, `^plugins\.[a-z0-9-]+\.enabled$`, path,
			"an entry that is not a plugin toggle cannot activate anything")
		paths = append(paths, path[:len(path)-len(".enabled")])
	}
	sort.Strings(paths)
	return paths
}

// TestWebBaselineTurnsOnExactlyTheDormantMembers is the whole promise of the
// package in one assertion. A starter that misses a member promises a baseline
// it does not deliver; one that enables something else claims a capability the
// application never selected. Both directions are failures, so both are
// compared against the Definitions the starter actually contributes.
func TestWebBaselineTurnsOnExactlyTheDormantMembers(t *testing.T) {
	dormant := dormantMemberPaths(t)
	require.NotEmpty(t, dormant,
		"the prelude is expected to have members that stay dormant; if that ever stops being true this test proves nothing")

	require.Equal(t, dormant, enabledPaths(t),
		"the baseline must switch on every dormant member of the Bundles it selects, and nothing else")
}

// TestWebBaselineBundlesAreNotStartableOnTheirOwn pins the engine boundary. A
// starter that selected an engine would have to live outside the transport/web
// module; this one deliberately does not, so the composition root's engine
// selection stays what makes the baseline startable.
func TestWebBaselineBundlesAreNotStartableOnTheirOwn(t *testing.T) {
	bundles := starter.Web().Bundles()
	require.Len(t, bundles, 1)

	entries := pluginmodel.BundleEntries(pluginmodel.Bundle(bundles[0]))
	require.NotEmpty(t, entries, "the baseline selects the prelude's Definitions")

	for _, entry := range entries {
		descriptor, ok := pluginmodel.DescribeDefinition(entry.Definition)
		require.True(t, ok)
		require.NotContains(t, descriptor.Key.String(), "gin",
			"the baseline names no engine: selecting one would pull its dependencies into the transport/web module")
	}
}

// TestWebBaselineDefaultsAreAFreshLayerPerCall pins the ownership of the
// returned map. A shared one would be mutable state behind an interface that
// claims to be composition data, and the mutation would surface as one App
// composing a baseline another App configured.
func TestWebBaselineDefaultsAreAFreshLayerPerCall(t *testing.T) {
	first := starter.Web().Defaults()
	second := starter.Web().Defaults()

	require.NotEmpty(t, first.Values)
	for path := range first.Values {
		delete(first.Values, path)
	}
	require.NotEmpty(t, second.Values, "a caller mutating one layer must not empty the next one")
	require.Equal(t, "web starter", second.Label,
		"the layer names its contributor, which is what doctor prints as the origin of a section")
}
