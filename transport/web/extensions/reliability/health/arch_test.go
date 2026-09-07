package health

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProtocolSpecificImportsStayInWebAdapter(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	for _, file := range files {
		if filepath.Ext(file) != ".go" || len(file) >= len("_test.go") && file[len(file)-len("_test.go"):] == "_test.go" {
			continue
		}
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		require.NoError(t, parseErr)
		for _, imported := range parsed.Imports {
			path, unquoteErr := strconv.Unquote(imported.Path.Value)
			require.NoError(t, unquoteErr)
			if path == "github.com/gin-gonic/gin" || path == "github.com/xbcio/xbc/transport/web" {
				assert.Equal(t, "web.go", filepath.Base(file), "%s must remain isolated to the Web adapter", path)
			}
		}
	}
}
