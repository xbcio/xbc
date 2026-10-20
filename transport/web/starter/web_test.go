package starter_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	pluginmodel "github.com/xbcio/xbc/plugin/model"
	"github.com/xbcio/xbc/transport/web/extensions/observability/accesslog"
	"github.com/xbcio/xbc/transport/web/extensions/observability/requestid"
	"github.com/xbcio/xbc/transport/web/extensions/reliability/timeout"
	"github.com/xbcio/xbc/transport/web/extensions/response/gzip"
	"github.com/xbcio/xbc/transport/web/extensions/security/securityheaders"
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

// maxBaselineBufferBytes is the largest response body a capability the
// baseline turns on may hold in memory per request. It matches the 1 MiB
// default gzip and timeout ship; the ceiling is a separate statement so a
// plugin default cannot raise the baseline's memory profile unexamined.
const maxBaselineBufferBytes = 1 << 20

// requireBoundedBufferDefault asserts that a buffering capability keeps its
// own DefaultConfig finite and within the reviewed ceiling. The baseline writes
// no sizing key, so this default is exactly what a starter user runs with.
func requireBoundedBufferDefault(t *testing.T, key string, maxBufferBytes int) {
	t.Helper()

	require.Greaterf(t, maxBufferBytes, 0,
		"plugins.%s buffers a response body; its DefaultConfig must bound the buffer", key)
	require.LessOrEqualf(t, maxBufferBytes, maxBaselineBufferBytes,
		"plugins.%s buffers a response body; its DefaultConfig must stay within the %d-byte baseline ceiling",
		key, maxBaselineBufferBytes)
}

// TestWebBaselineKeepsBufferingDefaultsBounded pins the memory side of the
// baseline. The defaults layer writes only enabled:true entries -- enabledPaths
// asserts that -- so a buffering middleware runs with whatever its own
// DefaultConfig declares. If one of those defaults rose above the reviewed
// ceiling, every starter user's per-request memory ceiling would rise with it,
// silently. The enabled set is derived from the defaults layer, and every
// member must be classified here: one this test has not reasoned about fails
// rather than passing unexamined.
func TestWebBaselineKeepsBufferingDefaultsBounded(t *testing.T) {
	for _, section := range enabledPaths(t) {
		key := strings.TrimPrefix(section, "plugins.")
		switch key {
		case gzip.Key.String():
			requireBoundedBufferDefault(t, key, gzip.DefaultConfig().MaxBufferBytes)
		case timeout.Key.String():
			requireBoundedBufferDefault(t, key, timeout.DefaultConfig().MaxBufferBytes)
		case requestid.Key.String(), accesslog.Key.String(), securityheaders.Key.String():
			// These members hold no response body, so they expose no
			// memory-proportional default to bound.
		default:
			require.Failf(t, "unclassified baseline member",
				"the baseline enables %q, but this test does not know whether it buffers a response body; "+
					"known bounded members are gzip and timeout, known unbuffered members are requestid, accesslog, and securityheaders",
				key)
		}
	}
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
