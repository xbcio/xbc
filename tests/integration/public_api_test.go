package integration_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/plugin"
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

// TestPublicAPIExecutesPrivateBundle exercises the API exactly as an
// external importer sees it: only exported identifiers, no access to package
// xbc's internals. The package's own white-box tests all live inside package
// xbc, where an unexported helper is as reachable as an exported one, so none
// of them can tell whether the path an external caller actually takes --
// xbc.New with an explicit Bundle, then App.Execute -- still works end to end.
func TestPublicAPIExecutesPrivateBundle(t *testing.T) {
	var factories atomic.Int32
	definition := plugin.Define("public-api-fixture", func(plugin.BuildContext) (*int, error) {
		factories.Add(1)
		value := 1
		return &value, nil
	})

	app, err := xbc.New(xbc.WithBundles(plugin.BundleOf(definition)))
	require.NoError(t, err)

	code, err := app.Execute(context.Background(), []string{
		"doctor", "--config", quietConfig(t),
	})
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Zero(t, factories.Load(), "doctor must validate the plan without invoking factories")

	code, err = app.Execute(context.Background(), []string{"doctor"})
	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "can only be called once")
}

// TestPublicRunAcceptsExplicitComposition pins Run's signature from the
// position an external importer occupies. It is the entry point the facade's
// documentation and every example point at, and it is the only one that owns
// process facilities on the application's behalf, so it must keep accepting the
// same Options as New: a Run that took no Option would force every explicitly
// composed application back to hand-written signal handling and os.Exit.
//
// Run is deliberately not called here -- it terminates the process. Only its
// type is asserted, exactly as TestPublicAppSurface asserts Execute's.
func TestPublicRunAcceptsExplicitComposition(t *testing.T) {
	want := reflect.TypeOf((func(...xbc.Option))(nil))
	assert.Equal(t, want, reflect.TypeOf(xbc.Run),
		"Run must accept a variadic Option list and return nothing")
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
