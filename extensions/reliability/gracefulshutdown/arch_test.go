package gracefulshutdown

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCapabilityNamesNoTransport is the load-bearing guard for this module's
// reason to exist. The programmatic Controller used to be a package of
// transport/web, bundled alongside an optional HTTP route contributor, which
// meant the whole Web runtime entered the dependency closure of any plugin
// that merely wanted to request a clean shutdown. Naming a transport here,
// from production code or from a test, would recreate that closure and
// silently undo the split.
//
// Test files are included on purpose: an import from a _test.go file still
// lands in this module's go.mod, which is the artifact consumers resolve.
func TestCapabilityNamesNoTransport(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files, "no Go files found; this guard would pass without checking anything")

	inspected := 0
	for _, file := range files {
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		require.NoError(t, parseErr)
		inspected++
		for _, imported := range parsed.Imports {
			path, unquoteErr := strconv.Unquote(imported.Path.Value)
			require.NoError(t, unquoteErr)
			assert.Falsef(t, strings.HasPrefix(path, "github.com/xbcio/xbc/transport/"),
				"%s imports %s; the gracefulshutdown capability must stay protocol-neutral", file, path)
		}
	}
	require.NotZero(t, inspected)
}
