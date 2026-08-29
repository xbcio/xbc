package integration_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/plugin/catalog"
)

func quietConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application.yml")
	err := os.WriteFile(path, []byte(`
log:
  console:
    enabled: false
  file:
    enabled: false
`), 0o600)
	require.NoError(t, err)
	return path
}

// TestPublicAPIExecutesPrivateSnapshot exercises the API exactly as an
// external importer sees it: only exported identifiers, no access to package
// xbc's internals. The package's own white-box tests all live inside package
// xbc, where an unexported helper is as reachable as an exported one, so none
// of them can tell whether the path an external caller actually takes --
// xbc.New with an xbc.Option, then App.Execute -- still works end to end.
func TestPublicAPIExecutesPrivateSnapshot(t *testing.T) {
	privateCatalog := catalog.New()
	snapshot, err := privateCatalog.Freeze()
	require.NoError(t, err)

	app, err := xbc.New(xbc.WithDefinitions(snapshot))
	require.NoError(t, err)

	code, err := app.Execute(context.Background(), []string{
		"doctor", "--config", quietConfig(t),
	})
	require.NoError(t, err)
	assert.Equal(t, 0, code)

	code, err = app.Execute(context.Background(), []string{"doctor"})
	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "can only be called once")
}

// TestPublicAppSurface pins App's exported method set to exactly Execute, with
// exactly that signature.
//
// App intentionally exposes only Execute. This test ensures that root-package
// implementation methods cannot become public accidentally through embedding
// or an API change.
//
// It is deliberately not the same check as tests/architecture's
// TestArchRootPublicAPIIsFrozen, which reads the root package's source with an
// AST walk. That guard sees declarations; this one sees the method set as the
// type system computes it. A method promoted into App through anonymous
// embedding has no FuncDecl anywhere in the root package, so the AST guard
// cannot see it at all -- reflection is the only thing that can.
func TestPublicAppSurface(t *testing.T) {
	typ := reflect.TypeOf((*xbc.App)(nil))
	require.Equal(t, 1, typ.NumMethod())
	method, ok := typ.MethodByName("Execute")
	require.True(t, ok)
	want := reflect.TypeOf((func(*xbc.App, context.Context, []string) (int, error))(nil))
	assert.Equal(t, want, method.Type)
}
