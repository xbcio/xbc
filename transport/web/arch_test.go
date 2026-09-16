package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
// source files must never import the root facade or low-level core implementation
// packages", a statement about what web's authors wrote, not about what ends
// up on disk once gin or any other dependency's own transitive graph is
// flattened. Using Deps here would produce a false negative the moment
// gin (or any future dependency) happens to import something that in turn
// imports the root package -- Deps would report that unrelated edge as if
// web itself had written the import, when web's own source never did.
// Conversely, a hypothetical "web must never depend on Gin, even
// transitively" guard would need the opposite judgment (Deps, not
// Imports).
type packageJSON struct {
	ImportPath   string
	Imports      []string
	TestImports  []string
	XTestImports []string
}

var packageExtensionPrefixes = []string{
	"github.com/xbcio/xbc/transport/web/extensions/authentication/apikey",
	"github.com/xbcio/xbc/transport/web/extensions/authorization/tenant",
	"github.com/xbcio/xbc/transport/web/extensions/observability/accesslog",
	"github.com/xbcio/xbc/transport/web/extensions/observability/auditlog",
	"github.com/xbcio/xbc/transport/web/extensions/observability/pprof",
	"github.com/xbcio/xbc/transport/web/extensions/observability/requestid",
	"github.com/xbcio/xbc/transport/web/extensions/response/biz",
	"github.com/xbcio/xbc/transport/web/extensions/response/gzip",
	"github.com/xbcio/xbc/transport/web/extensions/reliability/gracefulshutdown",
	"github.com/xbcio/xbc/transport/web/extensions/reliability/health",
	"github.com/xbcio/xbc/transport/web/extensions/reliability/ratelimit",
	"github.com/xbcio/xbc/transport/web/extensions/reliability/recovery",
	"github.com/xbcio/xbc/transport/web/extensions/reliability/timeout",
	"github.com/xbcio/xbc/transport/web/extensions/security/cors",
	"github.com/xbcio/xbc/transport/web/extensions/security/securityheaders",
}

// forbiddenDirectImport reports why importing dep from web would violate a
// package-layout design §9 guard, or "" if dep is fine.
func forbiddenDirectImport(importer, dep string) string {
	if dep == "github.com/xbcio/xbc" {
		return "web may not reverse import root facade (design §9 guard #11)"
	}
	if dep == "github.com/xbcio/xbc/plugin/autoload" && strings.HasSuffix(importer, "/autoload") {
		return ""
	}
	if dep == "github.com/xbcio/xbc/transport/web/extensions" || strings.HasPrefix(dep, "github.com/xbcio/xbc/transport/web/extensions/") {
		for _, prefix := range packageExtensionPrefixes {
			if dep == prefix || strings.HasPrefix(dep, prefix+"/") {
				return ""
			}
		}
		return "transport/web module packages may not import independently versioned extension modules; those modules implement package-owned contracts (design §9 guards #7/#10)"
	}
	for _, forbidden := range []struct {
		prefix string
		reason string
	}{
		{"github.com/xbcio/xbc/plugin/autoload", "only leaf autoload packages may mutate optional process composition"},
		{"github.com/xbcio/xbc/plugin/assembly", "web may not directly depend on low-level Plugin assembly implementation"},
		{"github.com/xbcio/xbc/runtime", "web may not directly depend on runtime orchestration implementation"},
		{"github.com/xbcio/xbc/internal", "web may not import core internal/*"},
	} {
		if dep == forbidden.prefix || strings.HasPrefix(dep, forbidden.prefix+"/") {
			return forbidden.reason + " (design §9 guards #7/#10)"
		}
	}
	return ""
}

// TestWebDoesNotDirectlyImportFacadeOrFrameworkInfrastructure is the package-
// layout architecture guard for the complete Web module. Only leaf autoload
// packages may import plugin/autoload; Plugin assembly, runtime process
// implementation, and reverse imports of the root facade remain forbidden.
func TestWebDoesNotDirectlyImportFacadeOrFrameworkInfrastructure(t *testing.T) {
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
				if reason := forbiddenDirectImport(pkg.ImportPath, dep); reason != "" {
					assert.Fail(t, "forbidden direct dependency",
						"%s's %s contains %q: %s", pkg.ImportPath, c.label, dep, reason)
				}
			}
		}
	}
	require.NotZero(t, checked, "go list -json ./... returned no web package, dependency direction check actually did not take effect")
}

// engineModulePaths lists the module root of every HTTP engine this repository
// has an adapter for. A new adapter adds its engine here; what the three guards
// below assert is that transport/web itself never grows a line of coupling to
// any of them.
var engineModulePaths = []string{
	"github.com/gin-gonic/gin",
}

// hasEnginePrefix reports whether dep is, or is rooted under, one of
// engineModulePaths -- e.g. "github.com/gin-gonic/gin/binding" and
// "github.com/gin-gonic/gin/codec/json" both count, not just the bare
// module path.
func hasEnginePrefix(dep string) string {
	for _, engine := range engineModulePaths {
		if dep == engine || strings.HasPrefix(dep, engine+"/") {
			return engine
		}
	}
	return ""
}

// TestWebNeverNamesAnHTTPEngine is the source-level guard: no file in this
// module, production or test, may write an engine's import path. Test files
// count -- an engine import there binds this module's own tests to one engine
// just as firmly as production code would.
func TestWebNeverNamesAnHTTPEngine(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go command is unavailable, skipping HTTP engine source guard")
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
				if engine := hasEnginePrefix(dep); engine != "" {
					assert.Fail(t, "forbidden HTTP engine import",
						"%s's %s contains %q, naming engine %q", pkg.ImportPath, c.label, dep, engine)
				}
			}
		}
	}
	require.NotZero(t, checked, "go list -json ./... returned no web package, HTTP engine source guard actually did not take effect")
}

// goModRequire is the subset of `go mod edit -json`'s output this guard reads.
type goModRequire struct {
	Require []struct {
		Path     string
		Version  string
		Indirect bool
	}
}

// TestWebGoModsDoNotRequireAnHTTPEngine is the manifest-level companion. A
// leftover require has no import edge for a source or closure check to find,
// yet it still pollutes downloads, upgrades and security scans.
//
// It walks every go.mod at or below transport/web, because the extension
// modules are separately published and each inherits its own requirements.
// engines/ is exempt by construction: an engine adapter module requiring its
// engine is the entire reason it exists.
func TestWebGoModsDoNotRequireAnHTTPEngine(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go command is unavailable, skipping HTTP engine manifest guard")
	}

	var manifests []string
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() != "go.mod" {
			return nil
		}
		for _, part := range strings.Split(filepath.ToSlash(filepath.Dir(path)), "/") {
			if part == "engines" {
				return nil
			}
		}
		manifests = append(manifests, path)
		return nil
	})
	require.NoError(t, err, "walking transport/web for go.mod files failed")
	require.NotEmpty(t, manifests, "found no go.mod under transport/web, HTTP engine manifest guard actually did not take effect")

	for _, manifest := range manifests {
		out, err := exec.Command("go", "mod", "edit", "-json", manifest).Output()
		require.NoError(t, err, "go mod edit -json %s failed", manifest)

		var mod goModRequire
		require.NoError(t, json.Unmarshal(out, &mod), "parsing go mod edit -json output for %s failed", manifest)

		for _, req := range mod.Require {
			if engine := hasEnginePrefix(req.Path); engine != "" {
				assert.Fail(t, "forbidden HTTP engine require",
					"%s requires %q, naming engine %q", manifest, req.Path, engine)
			}
		}
	}
}

// TestWebDependencyClosureExcludesAnyHTTPEngine is the load-bearing one. The
// other two can be satisfied by a module that reaches an engine through a
// package it legitimately imports; only the closure can say that no engine code
// is compiled into this module at all. Engine neutrality is this property --
// not the existence of a second adapter.
func TestWebDependencyClosureExcludesAnyHTTPEngine(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go command is unavailable, skipping HTTP engine dependency closure guard")
	}

	out, err := exec.Command("go", "list", "-deps", "./...").Output()
	require.NoError(t, err, "go list -deps ./... failed")

	// Emptiness has to be judged before the split, not after: strings.Split of
	// an empty string yields a one-element slice holding "", so a NotEmpty
	// assertion on the split result passes for output that lists nothing at all
	// and this guard would silently stop guarding.
	listed := strings.TrimSpace(string(out))
	require.NotEmpty(t, listed, "go list -deps ./... returned no dependencies, HTTP engine dependency closure guard actually did not take effect")

	for _, dep := range strings.Split(listed, "\n") {
		if engine := hasEnginePrefix(dep); engine != "" {
			assert.Fail(t, "forbidden HTTP engine in dependency closure",
				"production dependency closure contains %q, naming engine %q", dep, engine)
		}
	}
}
