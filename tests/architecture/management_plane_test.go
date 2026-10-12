package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
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

	// The gRPC management package builds its own listener with the standard
	// library instead of registering through Router.Management, so the call
	// shape above cannot see it. What that plane serves is still an
	// architectural decision, so the same test reads its route registrations
	// directly and holds them to the same rule from both directions.
	routes := archGRPCManagementRoutes(t, root)
	for path := range routes {
		assert.Truef(t, archGRPCManagementServes(path),
			"the gRPC management listener registers %q; the plane serves a small, deliberately chosen set of operator endpoints -- add the route here deliberately, or serve it on the gRPC listener", path)
		assert.Falsef(t, strings.Contains(path, "health"),
			"the gRPC management listener registers %q; health is a readiness signal for whoever routes traffic to this process and stays on the serving listener (the standard gRPC health service)", path)
	}
	assert.True(t, routes["/metrics"],
		"the gRPC management listener must register /metrics: it is the HTTP outlet of a gRPC-only composition")
	assert.True(t, routes["/debug/pprof/"],
		"the gRPC management listener must register the profiling subtree under its opt-in")
}

// archGRPCManagementRoutes collects the path literals the gRPC management
// package registers on its own mux. The scan looks for the registration call
// rather than a file: a Handle or HandleFunc whose first argument is a string
// literal is a route declaration and nothing else this package does.
func archGRPCManagementRoutes(t *testing.T, root string) map[string]bool {
	t.Helper()
	packageRoot := filepath.Join(root, "transport", "grpc", "extensions", "management")
	files := archParseProductionGoFiles(t, packageRoot)
	require.NotEmpty(t, files, "%s must contain production Go files", packageRoot)

	routes := make(map[string]bool)
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (selector.Sel.Name != "Handle" && selector.Sel.Name != "HandleFunc") {
				return true
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			path, err := strconv.Unquote(literal.Value)
			require.NoError(t, err, "unquoting the route literal %s", literal.Value)
			routes[path] = true
			return true
		})
	}

	// Positive control: the assertions above prove nothing if the scan matched
	// no registration at all, which is what a renamed method would produce.
	require.NotEmpty(t, routes, "no route registration was found in the gRPC management package; the scan is looking for the wrong call shape")
	return routes
}

// archGRPCManagementServes is the operator-endpoint rule for the gRPC
// management plane: the neutral registry's exposition, and the profiling
// subtree with every standard endpoint beneath it.
func archGRPCManagementServes(path string) bool {
	return path == "/metrics" || strings.HasPrefix(path, "/debug/pprof/")
}
