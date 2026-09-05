package webhook

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestModuleManifestHasNoLocalOrPlaceholderDependency(t *testing.T) {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	pseudo := regexp.MustCompile(`v\d+\.\d+\.\d+-\d{14}-[0-9a-f]+`)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "replace ") || line == "replace (" {
			t.Fatalf("go.mod contains local replacement: %q", line)
		}
		if strings.Contains(line, "github.com/xbcio/xbc") && (strings.Contains(line, "v0.0.0") || pseudo.MatchString(line)) {
			t.Fatalf("go.mod contains placeholder internal version: %q", line)
		}
	}
}

func TestOrdinaryImplementationDoesNotDeclareAutoload(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		source := string(data)
		if strings.Contains(source, "internal/autoload") || strings.Contains(source, "autoload.Declare(") {
			t.Fatalf("ordinary implementation file %s declares autoload composition", entry.Name())
		}
	}
}
