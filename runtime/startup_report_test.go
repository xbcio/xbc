package runtime

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/assembly"
	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/catalog"
)

// --- assembly helper -------------------------------------------------------
//
// Every renderer here takes *assembly.Instance / *assembly.Container
// values, so the tests need genuinely assembled Instances. assemble below
// builds a real *assembly.Container the same way package assembly's own
// container_harness_test.go does for its package, which is what lets these tests
// exercise the renderers without going anywhere near newApp or Execute.
//
// Note what that independence does NOT come from: these tests used to live in
// their own package (internal/startupreport), and the package doc claimed the
// boundary was what made App-free testing possible. It was not. Nothing
// obliges a test in package xbc to construct the package's main type, as this
// file demonstrates by not doing so. The boundary's only real effect was
// forcing bare and def to be copied here from harness_test.go, because test
// helpers do not cross package boundaries -- both are now shared with the
// rest of the package's tests instead of duplicated.

// fakeHost forwards plugin.RuntimeHost's four methods straight back to a
// *assembly.Container, mirroring assembly/container_harness_test.go's own
// fakeHost. It is built with a nil c and wired up right after assembly.New,
// since Options.Host must already be a RuntimeHost value before the Container it
// forwards to exists.
type fakeHost struct {
	c *assembly.Container
}

func (h *fakeHost) ProvideValue(typ reflect.Type, instance string, v any) {
	h.c.ProvideValue(typ, instance, v)
}

func (h *fakeHost) LookupValue(typ reflect.Type, instance string) (any, error) {
	return h.c.LookupValue(typ, instance)
}

func (h *fakeHost) InitializedPlugins() ([]plugin.Extension[any], error) {
	return h.c.InitializedPlugins()
}

// GoManaged runs fn synchronously; none of this file's tests exercise
// Context.Go/GoCritical's own async semantics, so a synchronous stand-in is
// enough to satisfy plugin.RuntimeHost.
func (h *fakeHost) GoManaged(id plugin.Identity, fn func(context.Context), critical bool) {
	fn(context.Background())
}

// assemble builds an *assembly.Container from defs, failing the
// test immediately on any assembly error. It returns the whole Container (not
// just Order()) because the tests below also need Disabled() and SoftMisses()
// from the same assembled run.
func assemble(t *testing.T, envData map[string]any, defs ...plugin.Definition) *assembly.Container {
	t.Helper()

	cat := catalog.New()
	for _, d := range defs {
		cat.Declare(d)
	}
	snap, err := cat.Freeze()
	require.NoError(t, err, "Freezing test catalog failed")

	env, err := config.NewEnvironment(envData, "XBC_REPORT_TEST_")
	require.NoError(t, err, "Failed to construct in-memory configuration environment")

	h := &fakeHost{}
	c := assembly.New(assembly.Options{
		Snapshot: snap,
		Env:      env,
		Host:     h,
		Logger:   log.Nop(),
	})
	h.c = c

	require.NoError(t, c.Assemble(), "The test plugin collection should always be able to assemble successfully")
	return c
}

// --- narrow capability fixtures --------------------------------------------
//
// A single fake implementing every lifecycle interface at once would be
// useless for capability-detection tests: every instance built from it would
// report every capability regardless of what capabilityTokens actually does.
// These fixtures are deliberately narrow.

// fullCaps implements every optional lifecycle capability capabilityTokens
// checks for. Its methods are declared in the REVERSE of lifecycle order
// (Stop first, ConfigPtr last) on purpose: if capabilityTokens' output order
// ever tracked source declaration order instead of its own fixed,
// hand-written sequence of type assertions, this fixture would be the one to
// expose it.
type fullCaps struct{ plugin.Base }

var (
	_ plugin.Configurable  = (*fullCaps)(nil)
	_ plugin.Initializer   = (*fullCaps)(nil)
	_ plugin.Migrator      = (*fullCaps)(nil)
	_ plugin.Runner        = (*fullCaps)(nil)
	_ plugin.TrafficOpener = (*fullCaps)(nil)
	_ plugin.Closer        = (*fullCaps)(nil)
)

func (*fullCaps) Stop(context.Context) error        { return nil }
func (*fullCaps) OpenTraffic(*plugin.Context) error { return nil }
func (*fullCaps) Start(*plugin.Context) error       { return nil }
func (*fullCaps) Migrate(*plugin.Context) error     { return nil }
func (*fullCaps) Init(*plugin.Context) error        { return nil }
func (*fullCaps) ConfigPtr() any                    { return &struct{}{} }

// initAndStopOnly implements exactly two of the six optional capabilities, to
// pin that skipped capabilities leave no gap or misordering in
// capabilityTokens' output -- only "init" and "stop" should ever appear for
// it, in that relative order.
type initAndStopOnly struct{ plugin.Base }

var (
	_ plugin.Initializer = (*initAndStopOnly)(nil)
	_ plugin.Closer      = (*initAndStopOnly)(nil)
)

func (*initAndStopOnly) Init(*plugin.Context) error { return nil }
func (*initAndStopOnly) Stop(context.Context) error { return nil }

// migratorOnly implements only Migrator, used to build a mixed set of
// instances for renderMigrationNotice's count without dragging in every other
// capability fullCaps carries.
type migratorOnly struct{ plugin.Base }

var _ plugin.Migrator = (*migratorOnly)(nil)

func (*migratorOnly) Migrate(*plugin.Context) error { return nil }

// softAfterMiss declares a soft After dependency on a Definition key that is
// never registered in any of this file's fixtures, to exercise
// renderSoftMisses' "After" rendering path.
type softAfterMiss struct{ plugin.Base }

var _ plugin.Declarer = (*softAfterMiss)(nil)

func (*softAfterMiss) Dependencies() plugin.Deps {
	return plugin.Deps{After: []plugin.Key{"ghost"}}
}

// softBeforeMiss mirrors softAfterMiss for the "Before" direction.
type softBeforeMiss struct{ plugin.Base }

var _ plugin.Declarer = (*softBeforeMiss)(nil)

func (*softBeforeMiss) Dependencies() plugin.Deps {
	return plugin.Deps{Before: []plugin.Key{"phantom"}}
}

// --- tests ------------------------------------------------------------------

// TestCapabilityTokensListsInLifecycleOrder pins capabilityTokens' central
// promise: the tokens it returns are always in lifecycle order (config, init,
// migrate, runner, traffic, stop), regardless of which subset of capabilities
// an instance actually has and regardless of the order its methods happen to
// be declared in source.
func TestCapabilityTokensListsInLifecycleOrder(t *testing.T) {
	c := assemble(t, nil,
		def("full", func() plugin.Plugin { return new(fullCaps) }),
		def("sparse", func() plugin.Plugin { return new(initAndStopOnly) }),
	)

	var full, sparse *assembly.Instance
	for _, inst := range c.Order() {
		switch inst.Label() {
		case "full":
			full = inst
		case "sparse":
			sparse = inst
		}
	}
	require.NotNil(t, full, "Full instance not found")
	require.NotNil(t, sparse, "Sparse instance not found")

	assert.Equal(t, []string{"config", "init", "migrate", "runner", "traffic", "stop"},
		capabilityTokens(full),
		"An instance implementing all capability interfaces must have token order strictly following the lifecycle order,"+
			"not the method declaration order in the source code (fullCaps methods are intentionally declared in reverse order)")

	assert.Equal(t, []string{"init", "stop"}, capabilityTokens(sparse),
		"When implementing only partial capability interfaces, missing capabilities should not leave empty slots or cause misalignment in the token list")
}

// TestRenderersNeverEmitRemovedDesignTokens pins the absence of every token
// that belonged to a design this framework has already moved past:
// "health"/"middleware" belong to capabilities that were split out to the web
// module, and "routes(N)"/"↑ Explicit Register" belong to the deleted
// dual-registration era. Any one of them reappearing in the report is a
// regression, not a stylistic choice, so this exercises every renderer
// together against one assembled run that also has a disabled plugin, a soft
// miss, and a skipped migration -- the three other conditions that grow extra
// report lines.
func TestRenderersNeverEmitRemovedDesignTokens(t *testing.T) {
	c := assemble(t, nil,
		def("full", func() plugin.Plugin { return new(fullCaps) }),
		def("soft_after", func() plugin.Plugin { return &softAfterMiss{} }),
		plugin.Definition{
			Key:        "shelved",
			Factory:    func() plugin.Plugin { return &bare{} },
			Activation: plugin.Configured("plugins.shelved"),
		},
	)

	combined := strings.Join([]string{
		renderInstanceTable(c.Order()),
		renderDisabled(c.Disabled()),
		renderSoftMisses(c.SoftMisses()),
		renderMigrationNotice(c.Order(), false),
	}, "\n")

	for _, removed := range []string{"health", "middleware", "routes(", "↑ Explicit Register"} {
		assert.NotContains(t, combined, removed,
			"An old design token %q was found in the report output: health/middleware belongs to the split capability assigned to the web module,"+
				"routes(N) and \"↑ Explicit Register\" belong to the dual registration era, the reappearance of either in the core report indicates a regression",
			removed)
	}
}

// TestRenderInstanceTableColumnWidthAdaptsToLongestLabel pins that column
// widths are computed from this run's actual content, not from any fixed
// number. The discriminating check is on the short-named row: it must be
// padded to exactly the long name's width, computed independently in the test
// rather than copied from renderInstanceTable's own labelWidth variable, so a
// hardcoded-width regression in renderInstanceTable would make this row
// disagree with the expected string built here.
func TestRenderInstanceTableColumnWidthAdaptsToLongestLabel(t *testing.T) {
	longName := strings.Repeat("q", 61)

	c := assemble(t, nil,
		def("a", func() plugin.Plugin { return new(fullCaps) }),
		def(longName, func() plugin.Plugin { return &bare{} }),
	)
	order := c.Order()
	require.Len(t, order, 2, "Expected exactly two instances")

	var short, long *assembly.Instance
	for _, inst := range order {
		switch inst.Label() {
		case "a":
			short = inst
		case longName:
			long = inst
		}
	}
	require.NotNil(t, short, "Short name instance a not found")
	require.NotNil(t, long, "Long name instance not found")

	shortCaps := strings.Join(capabilityTokens(short), " ")
	require.NotEmpty(t, shortCaps, "fullCaps should report at least one capability, otherwise this column width pin test lacks discriminative power")

	labelWidth := len(longName)
	wantShortLine := "  a" + strings.Repeat(" ", labelWidth-len("a")) + "  " + shortCaps
	wantLongLine := "  " + longName

	table := renderInstanceTable(order)
	lines := strings.Split(table, "\n")
	require.Len(t, lines, 3, "Should be header + two instance rows, totaling three rows")

	assert.Contains(t, lines, wantShortLine,
		"Short name row must be filled to the same column width (%d) as the long name, rather than a fixed width—"+
			"If the implementation changes to a hard-coded width, the fill length of this row will differ from the expected value calculated here", labelWidth)
	assert.Contains(t, lines, wantLongLine,
		"The longest name itself defines the column width, and this row should not have any extra padding")
}

// TestRenderDisabledListsDeclaredButNotEnabledPlugins pins renderDisabled's
// two jobs: naming every declared-but-inactive plugin, and saying plainly
// that this is not an error.
func TestRenderDisabledListsDeclaredButNotEnabledPlugins(t *testing.T) {
	c := assemble(t, nil,
		def("always_on", func() plugin.Plugin { return &bare{} }),
		plugin.Definition{
			Key:        "shelved",
			Factory:    func() plugin.Plugin { return &bare{} },
			Activation: plugin.Configured("plugins.shelved"),
		},
	)

	out := renderDisabled(c.Disabled())
	assert.Contains(t, out, "shelved", "The list of disabled plugins should include shelved")
	assert.Contains(t, out, "Being disabled is not an error", "Should clearly inform the reader that disabled is not an error")
	assert.NotContains(t, out, "always_on", "Enabled plugins should not appear in the disabled list")
}

// TestRenderSoftMissesRendersDirectionsWithHints pins renderSoftMisses' exact
// rendering of both directions: Dir is stored lowercase ("after"/"before") on
// ordering.Miss but must render capitalized, and every entry must carry the
// "typo or forgot to enable" hint naming the specific missing plugin's config
// path.
func TestRenderSoftMissesRendersDirectionsWithHints(t *testing.T) {
	c := assemble(t, nil,
		def("after_one", func() plugin.Plugin { return &softAfterMiss{} }),
		def("before_one", func() plugin.Plugin { return &softBeforeMiss{} }),
	)

	misses := c.SoftMisses()
	require.Len(t, misses, 2, "Two instances each declare a soft constraint pointing to a non-existent plugin")

	out := renderSoftMisses(misses)

	assert.Contains(t, out, `after_one.After = "ghost"`,
		"After-direction soft constraints should be rendered as Node.After = \"Ref\" with the direction word capitalized")
	assert.Contains(t, out, "forgot to enable plugins.ghost",
		"After miss should give a hint of \"Typo? Or forgot to enable\" and point to the specific plugins.* path")

	assert.Contains(t, out, `before_one.Before = "phantom"`,
		"Before-direction soft constraints should be rendered as Node.Before = \"Ref\" with the direction word capitalized")
	assert.Contains(t, out, "forgot to enable plugins.phantom",
		"Before miss should also give a hint and point to the specific plugins.* path")
}

// TestRenderMigrationNotice pins renderMigrationNotice's three cases:
// migrate=true always suppresses the notice regardless of how many Migrators
// exist; migrate=false reports the exact count of instances that implement
// plugin.Migrator, not the total instance count; and zero Migrators produces
// no notice even when migrate=false.
func TestRenderMigrationNotice(t *testing.T) {
	c := assemble(t, nil,
		def("has_migrate_1", func() plugin.Plugin { return new(fullCaps) }),
		def("has_migrate_2", func() plugin.Plugin { return &migratorOnly{} }),
		def("plain", func() plugin.Plugin { return &bare{} }),
	)
	order := c.Order()
	require.Len(t, order, 3, "Expected three instances: two implement Migrator and one does not")

	assert.Equal(t, "", renderMigrationNotice(order, true),
		"When migrate=true, migration has already run, so no notice should be shown regardless of how many instances implement Migrator")

	note := renderMigrationNotice(order, false)
	assert.Contains(t, note, "(2 plugins declare Migrate",
		"Should count instances implementing Migrator (2), not all instances (3)")
	assert.Contains(t, note, "--migrate", "Should explain how to run migrations manually")

	noMigrators := assemble(t, nil, def("plain_only", func() plugin.Plugin { return &bare{} }))
	assert.Equal(t, "", renderMigrationNotice(noMigrators.Order(), false),
		"No migration notice should be produced when no instance implements Migrator, even if migrate=false")
}
