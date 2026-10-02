package async

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
// reason to exist as a protocol-neutral extension: a background task pool
// usable by a request handler, a background service, or a maintenance task
// alike must not pull in a transport. Naming one, from production code or
// from a test, would silently narrow this capability's audience to whichever
// transport it imported.
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
				"%s imports %s; the async capability must stay protocol-neutral", file, path)
		}
	}
	require.NotZero(t, inspected)
}
