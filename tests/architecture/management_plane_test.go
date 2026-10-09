package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// archManagementScopedPackages are the only packages allowed to register routes
// on the management plane. The plane is a second listener an operator may
// configure, and its whole point is that it serves a small, deliberately
// chosen set of operator endpoints and nothing an ordinary client depends on.
// Which endpoints those are is therefore an architectural decision, not a
// per-plugin preference: admitting a package here moves its routes off the
// serving port, where the rest of the deployment -- service discovery, the
// ingress, the authentication policy compiled for serving routes -- still
// expects them.
//
// The list holds the operator endpoints. Health and readiness are deliberately
// absent and asserted absent below: they are consulted by whatever decides
// whether to send traffic to this process, which is the serving port's job.
// Moving them would make a probe that cannot reach the serving plane read a
// deliberate "not ready" as "process is gone".
var archManagementScopedPackages = map[string]bool{
	"transport/web/extensions/observability/metrics": true,
	"transport/web/extensions/observability/pprof":   true,
}

// TestArchManagementPlaneIsConfinedToOperatorEndpoints holds the set of
// packages that register on the management plane to the list above in both
// directions: a package that starts using the plane fails until it is admitted
// deliberately, and a name in the list that no longer uses the plane fails so
// the list cannot read as coverage it does not have.
//
// The scan looks for the call shape, not the name: a zero-argument
// .Management() call is Router.Management's derivation and nothing else in the
// repository shares it. Selector expressions that read a field or call a method
// named Management on some other type would match the identifier but not this
// shape, so the guard does not fail on prose or on unrelated APIs.
func TestArchManagementPlaneIsConfinedToOperatorEndpoints(t *testing.T) {
	root := archRepositoryRoot(t)
	found := make(map[string]int)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		require.NoError(t, walkErr)
		if entry.IsDir() {
			// Tool droppings are skipped for the same reason as .git and vendor:
			// nothing under them is compiled by any package pattern, so a guard
			// on who may register on the management plane must not read them.
			if archIgnoresDirectoryEntry(entry.Name()) || entry.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		relative, err := filepath.Rel(root, filepath.Dir(path))
		require.NoError(t, err)
		owner := filepath.ToSlash(relative)
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) != 0 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if ok && selector.Sel.Name == "Management" {
				found[owner]++
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)

	// Positive control: the comparison below proves nothing if the scan matched
	// no call at all, which is what a renamed method would produce.
	require.NotEmpty(t, found, "no zero-argument .Management() call was found; the scan is looking for the wrong call shape")

	for owner := range found {
		assert.Truef(t, archManagementScopedPackages[owner],
			"%s registers routes on the management plane; the plane is served on a listener an ordinary client does not reach, so admitting a package to it is an architectural decision -- add it to archManagementScopedPackages deliberately, or register on the serving router",
			owner)
	}
	for owner := range archManagementScopedPackages {
		assert.NotZerof(t, found[owner],
			"archManagementScopedPackages names %s, which registers nothing on the management plane; the list overstates its coverage", owner)
	}

	assert.False(t, archManagementScopedPackages["transport/web/extensions/reliability/health"],
		"health is a readiness signal for whoever routes traffic to this process, not an operator endpoint")
	assert.NotContains(t, found, "transport/web/extensions/reliability/health",
		"the health endpoint must stay on the serving listener: a deployment where the traffic router consults readiness, and the probe that answers it lives on a second port, is a deployment whose probes cannot reach the process that depends on them")
}
