package architecture_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// archProtocolClientModules is the complete, intentional set of protocol
// client modules: modules that implement one external protocol and publish a
// client API, but own no plugin Definition, config section, or Bundle. Most
// wrap the third-party library that does the protocol work -- OIDC's JOSE and
// JWKS handling, for example; the OAuth 2.0 authorization-code client
// implements its protocol directly and carries no third-party dependency.
//
// They are neither contract modules (they implement a protocol instead of
// defining shared types) nor plugin implementations (no composition root
// selects them through a plugin.Bundle; a transport adapter calls them). The
// plugin-implementation guards therefore skip them, and
// TestArchProtocolClientModulesOwnNoPluginSurface is what makes that exclusion
// safe to grant. The workspace, release-manifest, Web-zone, and per-module
// closure guards still cover every member.
//
// Adding a member is an architecture decision: a protocol client is the
// neutral half of the "protocol module + transport adapter" split, so it must
// live beneath extensions and stay free of every transport.
var archProtocolClientModules = []string{
	"extensions/authentication/oauth2",
	"extensions/authentication/oidc",
}

// archNonPluginModuleRoots returns the absolute path of every module declared
// to be neither a contract module nor a plugin implementation, keyed for
// exclusion lookups.
func archNonPluginModuleRoots(repositoryRoot string) map[string]bool {
	roots := archContractModuleRoots(repositoryRoot)
	for _, path := range archProtocolClientModules {
		roots[filepath.Join(repositoryRoot, filepath.FromSlash(path))] = true
	}
	return roots
}

// TestArchProtocolClientModulesOwnNoPluginSurface keeps the category honest: a
// module listed here is an independently versioned module beneath extensions
// with production files that never imports the plugin package or a transport.
// Without this guard the exclusion granted in archPluginImplementationRoots
// would let a module escape the plugin-implementation checks while still
// claiming a plugin or a transport surface.
func TestArchProtocolClientModulesOwnNoPluginSurface(t *testing.T) {
	t.Parallel()
	repositoryRoot := archRepositoryRoot(t)
	for _, relative := range archProtocolClientModules {
		relative := relative
		t.Run(relative, func(t *testing.T) {
			t.Parallel()
			assert.Truef(t, strings.HasPrefix(relative, "extensions/"),
				"%s must live beneath the extensions namespace; a protocol client is the neutral half of a protocol module", relative)

			moduleRoot := filepath.Join(repositoryRoot, filepath.FromSlash(relative))
			info, err := os.Stat(filepath.Join(moduleRoot, "go.mod"))
			require.NoErrorf(t, err, "%s must be an independently versioned module", relative)
			require.Falsef(t, info.IsDir(), "%s/go.mod must be a file", relative)

			files := archParseProductionGoFiles(t, moduleRoot)
			require.NotEmptyf(t, files, "%s must contain production Go files", relative)
			for _, file := range files {
				for _, specification := range file.Imports {
					importPath, err := strconv.Unquote(specification.Path.Value)
					require.NoError(t, err)
					assert.Falsef(t, archPathAtOrBelow(importPath, archPluginImportPath),
						"%s imports %q; a protocol client owns no plugin Definition, Config, or Bundle", relative, importPath)
					assert.Falsef(t, archPathAtOrBelow(importPath, archTransportNamespace),
						"%s imports %q; a protocol client must stay transport-neutral", relative, importPath)
				}
			}
		})
	}
}
