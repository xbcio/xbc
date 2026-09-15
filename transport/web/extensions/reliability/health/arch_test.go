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

// TestProtocolSpecificImportsStayInWebAdapter keeps the health capability
// itself protocol-neutral: only the Web adapter file may name the Web
// transport. The rule used to also list gin, but transport/web no longer
// exposes an engine type, so a gin clause here would be a condition nothing can
// satisfy -- it would keep passing while guarding nothing. The live condition
// is the transport import, and the counter below keeps it from passing
// vacuously if the adapter ever stops importing the transport at all.
func TestProtocolSpecificImportsStayInWebAdapter(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	adapterImports := 0
	for _, file := range files {
		if filepath.Ext(file) != ".go" || len(file) >= len("_test.go") && file[len(file)-len("_test.go"):] == "_test.go" {
			continue
		}
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		require.NoError(t, parseErr)
		for _, imported := range parsed.Imports {
			path, unquoteErr := strconv.Unquote(imported.Path.Value)
			require.NoError(t, unquoteErr)
			if path == "github.com/xbcio/xbc/transport/web" {
				adapterImports++
				assert.Equal(t, "web.go", filepath.Base(file), "%s must remain isolated to the Web adapter", path)
			}
		}
	}
	require.NotZero(t, adapterImports, "no production file imports the Web transport; this guard would pass without checking anything")
}
