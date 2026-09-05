package elasticsearch

import (
	"os"
	"strings"
	"testing"
)

func TestModuleManifestPinsElasticsearchAndHasNoLocalCorePlaceholder(t *testing.T) {
	contents, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("ReadFile(go.mod) error = %v", err)
	}
	manifest := string(contents)
	if !strings.Contains(manifest, "github.com/elastic/go-elasticsearch/v8 v8.19.1") {
		t.Fatal("go.mod does not pin go-elasticsearch v8.19.1")
	}
	for _, line := range strings.Split(manifest, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "replace ") ||
			(strings.HasPrefix(trimmed, "github.com/xbcio/xbc ") &&
				(strings.Contains(trimmed, "v0.0.0") || strings.Contains(trimmed, "-20"))) {
			t.Fatalf("go.mod contains forbidden directive %q", trimmed)
		}
	}
}

func TestOrdinaryImplementationDoesNotDeclareAutoload(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		contents, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(contents), "autoload.Declare") {
			t.Fatalf("ordinary implementation file %s declares autoload composition", entry.Name())
		}
	}
}
