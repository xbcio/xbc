package assembly

import (
	"context"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/catalog"
)

// fakeHost forwards RuntimeHost's four methods straight back to a *Container,
// mirroring the bootstrap order the root package's real hostAdapter is
// expected to follow: a Context never talks to a Container directly, only
// through this narrow port. It is built with a nil c and wired up right
// after New, since Options.Host must already be a RuntimeHost value before the
// Container it forwards to has been constructed.
type fakeHost struct {
	c *Container
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

// GoManaged runs fn synchronously on the calling goroutine. None of this
// package's tests exercise Context.Go/GoCritical's own async semantics --
// that belongs to whatever runs the real managed task group -- so a
// synchronous stand-in is enough to satisfy the RuntimeHost interface.
func (h *fakeHost) GoManaged(id plugin.Identity, fn func(context.Context), critical bool) {
	fn(context.Background())
}

// freezeDefs builds an isolated catalog.Snapshot from defs, failing the test
// immediately on a Freeze error -- every fixture Definition in this
// package's tests is expected to be well-formed, so a Freeze failure here
// always means the test itself is broken, never the code under test.
func freezeDefs(t *testing.T, defs ...plugin.Definition) catalog.Snapshot {
	t.Helper()
	cat := catalog.New()
	for _, d := range defs {
		cat.Declare(d)
	}
	snap, err := cat.Freeze()
	require.NoError(t, err, "the test plugin directory must always freeze successfully")
	return snap
}

// newTestEnv builds a config.Environment directly from a nested map,
// bypassing file/ENV loading entirely.
func newTestEnv(t *testing.T, data map[string]any) *config.Environment {
	t.Helper()
	env, err := config.NewEnvironment(data, "XBC_TEST_")
	require.NoError(t, err)
	return env
}

// newTestContainer wires up a *Container together with a fakeHost that
// forwards straight back to it.
func newTestContainer(t *testing.T, snap catalog.Snapshot, envData map[string]any) *Container {
	t.Helper()
	h := &fakeHost{}
	c := New(Options{
		Snapshot: snap,
		Env:      newTestEnv(t, envData),
		Host:     h,
		Logger:   log.Nop(),
	})
	h.c = c
	return c
}
