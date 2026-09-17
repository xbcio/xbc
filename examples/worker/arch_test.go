package main

import (
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

// TestWorkerSelectsNoTransport is what makes this example's claim verifiable
// rather than merely stated. Its whole purpose is to demonstrate that XBC drives
// a lifecycle, configuration, and health for an application with no transport
// at all, so a later edit that reaches for transport/web -- to expose "just one"
// endpoint -- would quietly turn it into a second Web example and leave the
// background-only path undemonstrated again.
//
// The walk covers this command and its internal packages, because the plugins
// under internal/ are where a transport import would actually be tempting.
func TestWorkerSelectsNoTransport(t *testing.T) {
	inspected := 0
	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		require.NoError(t, parseErr)
		inspected++
		for _, imported := range parsed.Imports {
			importPath, unquoteErr := strconv.Unquote(imported.Path.Value)
			require.NoError(t, unquoteErr)
			assert.Falsef(t, strings.HasPrefix(importPath, "github.com/xbcio/xbc/transport/"),
				"%s imports %s; the worker example exists to show a background-only service", path, importPath)
		}
		return nil
	})
	require.NoError(t, err)
	require.NotZero(t, inspected, "no Go files found; this guard would pass without checking anything")
}
