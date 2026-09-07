package tenant

import (
	"os"
	"strings"
	"testing"
)

func TestCatalogDeclarationExistsOnlyInAutoload(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "catalog.Declare") {
			t.Fatalf("ordinary implementation file %s registers globally", entry.Name())
		}
	}
}
