package web

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// packageJSON is the subset of `go list -json`'s output this guard reads.
// Imports/TestImports/XTestImports are each package's own *direct* import
// list -- as declared in its own source files' import blocks -- as opposed
// to Deps, which is the full transitive closure reachable from the package.
//
// This guard deliberately checks Imports/TestImports/XTestImports, not
// Deps, per package-layout design §9.1: the rule under test is "web's own
// source files must never bypass public capability owners to import the root
// facade or lower-level core owner packages", a statement about what web's
// authors wrote, not about what
// ends up on disk once gin or any other dependency's own transitive graph
// is flattened. Using Deps here would produce a false negative the moment
// gin (or any future dependency) happens to import something that in turn
// imports the root package -- Deps would report that unrelated edge as if
// web itself had written the import, when web's own source never did.
// Conversely, a hypothetical "web must never depend on Gin, even
// transitively" guard would need the opposite judgment (Deps, not
// Imports) -- the two kinds of guard are not interchangeable, and this
// package only ever needs the direct-import kind.
type packageJSON struct {
	ImportPath   string
	Imports      []string
	TestImports  []string
	XTestImports []string
}

// forbiddenDirectImport reports why importing dep from web would violate a
// package-layout design §9 guard, or "" if dep is fine.
func forbiddenDirectImport(dep string) string {
	if dep == "github.com/xbcio/xbc" {
		return "web may not reverse import root facade (design §9 guard #11)"
	}
	for _, forbidden := range []struct {
		prefix string
		reason string
	}{
		{"github.com/xbcio/xbc/runtime", "web may not directly depend on runtime orchestration implementation"},
		{"github.com/xbcio/xbc/assembly", "web may not directly depend on instance assembly implementation"},
		{"github.com/xbcio/xbc/cli", "web may not directly depend on command parsing implementation"},
		{"github.com/xbcio/xbc/internal", "web may not import core internal/*"},
	} {
		if dep == forbidden.prefix || strings.HasPrefix(dep, forbidden.prefix+"/") {
			return forbidden.reason + " (design §9 guards #7/#10)"
		}
	}
	return ""
}

// TestWebDoesNotDirectlyImportFacadeOrHigherLevelOwners is the
// package-layout design §9 architecture guard for the complete web module:
// neither web, autoload, nor a future subpackage's production/test sources may
// directly import the root facade, runtime, assembly, cli, or core internal
// packages. See packageJSON's doc comment for why this reads direct imports
// rather than Deps.
func TestWebDoesNotDirectlyImportFacadeOrHigherLevelOwners(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go command is unavailable, skipping dependency direction check")
	}

	out, err := exec.Command("go", "list", "-json", "./...").Output()
	require.NoError(t, err, "go list -json ./... failed")

	dec := json.NewDecoder(strings.NewReader(string(out)))
	checked := 0
	for dec.More() {
		var pkg packageJSON
		require.NoError(t, dec.Decode(&pkg), "Parsing go list -json output failed")
		checked++

		checks := []struct {
			label   string
			imports []string
		}{
			{"production code import", pkg.Imports},
			{"package internal _test.go import", pkg.TestImports},
			{"external test package import", pkg.XTestImports},
		}
		for _, c := range checks {
			for _, dep := range c.imports {
				if reason := forbiddenDirectImport(dep); reason != "" {
					assert.Fail(t, "forbidden direct dependency",
						"%s's %s contains %q: %s", pkg.ImportPath, c.label, dep, reason)
				}
			}
		}
	}
	require.NotZero(t, checked, "go list -json ./... returned no web package, dependency direction check actually did not take effect")
}
