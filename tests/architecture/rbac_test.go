package architecture_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	archBusinessRBACImportPath = "github.com/xbcio/xbc/extensions/authorization/rbac"
	archWebRBACImportPath      = "github.com/xbcio/xbc/transport/web/extensions/authorization/rbac"
)

// TestArchRBACOwnershipBoundaries keeps business authorization independent of
// any transport while allowing the Web package to adapt that capability to
// native middleware. The checks intentionally constrain dependency direction
// and public ownership rather than the adapter's internal file layout.
func TestArchRBACOwnershipBoundaries(t *testing.T) {
	t.Run("business plugin is protocol neutral", func(t *testing.T) {
		for _, dep := range archDeps(t, "./extensions/authorization/rbac/...") {
			for _, forbidden := range []string{
				"github.com/gin-gonic/gin",
				"github.com/xbcio/xbc/transport",
			} {
				if archPathAtOrBelow(dep, forbidden) {
					t.Errorf("extensions/authorization/rbac production closure contains %q: the business RBAC plugin must remain protocol neutral", dep)
				}
			}
		}
	})

	t.Run("web package is middleware adapter only", func(t *testing.T) {
		packages := archGoList(t, "./transport/web/extensions/authorization/rbac")
		require.Len(t, packages, 1, "transport/web/extensions/authorization/rbac must remain one Web adapter package")
		assert.Equal(t, archWebRBACImportPath, packages[0].ImportPath)
		productionImports := packages[0].Imports
		assert.Contains(t, productionImports, archBusinessRBACImportPath,
			"the Web RBAC adapter must consume the protocol-neutral business contract")

		// The adapter speaks the Web transport contract, never a concrete
		// engine. Two ways in have to be closed, and they need different
		// evidence.
		//
		// Reaching an engine adapter module under transport/web/engines is the
		// indirect way, and only the closure can see it, since it can arrive
		// through a package the adapter legitimately imports.
		for _, dep := range archDeps(t, "./transport/web/extensions/authorization/rbac") {
			if archPathAtOrBelow(dep, "github.com/xbcio/xbc/transport/web/engines") {
				t.Errorf("transport/web/extensions/authorization/rbac production closure contains engine adapter %q: middleware must depend on the neutral Web contract only", dep)
			}
		}

		// Writing `import "github.com/gin-gonic/gin"` straight into the adapter
		// is the direct way, and the closure above cannot answer it: transport/web
		// itself still compiles Gin in, so Gin is in this adapter's closure no
		// matter what the adapter's own files say, and a closure judgment would
		// be permanently red for a reason that is not this rule. The answerable
		// question -- the one this boundary exists for -- is whether the adapter
		// reaches for the engine itself. Test files count: a Gin import there
		// binds the adapter's own tests to one engine just as firmly.
		for _, dep := range archDirectImports(packages[0]) {
			if archPathAtOrBelow(dep, "github.com/gin-gonic/gin") {
				t.Errorf("transport/web/extensions/authorization/rbac directly imports %q: middleware must depend on the neutral Web contract only, never on a concrete engine", dep)
			}
		}

		for _, dep := range productionImports {
			if !archPathAtOrBelow(dep, archRootPackage) {
				continue
			}
			if dep == archBusinessRBACImportPath || dep == "github.com/xbcio/xbc/transport/web" {
				continue
			}
			t.Errorf("transport/web/extensions/authorization/rbac directly imports repository package %q: the adapter may depend only on extensions/authorization/rbac and the Web transport contract", dep)
		}

		adapterRoot := filepath.Join(archRepositoryRoot(t), "transport", "web", "extensions", "authorization", "rbac")
		assert.Equal(t, []string{"RequireAll", "RequireAny"}, archExportedNamesInDir(t, adapterRoot),
			"transport/web/extensions/authorization/rbac must expose middleware adapters, not own RBAC configuration, policy contracts, or plugin composition")
		_, err := os.Stat(filepath.Join(adapterRoot, "autoload"))
		require.ErrorIs(t, err, os.ErrNotExist,
			"transport/web/extensions/authorization/rbac must not own autoload; compose extensions/authorization/rbac independently")
	})
}
