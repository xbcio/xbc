package asynq

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestModuleManifestPinsDependenciesWithoutLocalCorePlaceholder(t *testing.T) {
	contents, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("ReadFile(go.mod) error = %v", err)
	}
	manifest := string(contents)
	for _, dependency := range []string{
		"github.com/hibiken/asynq v0.26.0",
		"github.com/redis/go-redis/v9 v9.21.0",
		"github.com/alicebob/miniredis/v2 v2.38.0",
	} {
		if !strings.Contains(manifest, dependency) {
			t.Fatalf("go.mod does not pin %q", dependency)
		}
	}
	// github.com/xbcio/xbc resolves through its published release tag; a local
	// substitute or a placeholder version would break publication. The check is
	// line-based so it holds for both spellings tidy uses: a bare entry inside
	// a require block and a single-line "require path version".
	pseudoVersion := regexp.MustCompile(`v\d+\.\d+\.\d+-\d{14}-[0-9a-f]+`)
	for _, line := range strings.Split(manifest, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "replace ") {
			t.Fatalf("go.mod contains forbidden replace directive %q", trimmed)
		}
		if strings.Contains(trimmed, "github.com/xbcio/xbc") &&
			(strings.Contains(trimmed, "v0.0.0") || pseudoVersion.MatchString(trimmed)) {
			t.Fatalf("go.mod contains forbidden internal dependency directive %q", trimmed)
		}
	}
}

func TestOrdinaryImplementationDoesNotDeclareAutoloadComposition(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		contents, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		source := string(contents)
		if strings.Contains(source, "plugin/autoload") || strings.Contains(source, "autoload.Declare(") {
			t.Fatalf("ordinary implementation file %s declares autoload composition", entry.Name())
		}
	}
}
