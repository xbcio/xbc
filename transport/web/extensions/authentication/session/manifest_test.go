package session

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestModuleManifestIsPublishableAndPinsThirdPartyDependencies(t *testing.T) {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	manifest := string(data)
	for _, expected := range []string{
		"go 1.25.0",
		"github.com/alicebob/miniredis/v2 v2.38.0",
		"github.com/redis/go-redis/v9 v9.21.0",
	} {
		if !strings.Contains(manifest, expected) {
			t.Fatalf("go.mod does not contain exact dependency %q", expected)
		}
	}
	if regexp.MustCompile(`(?m)^\s*replace\b`).MatchString(manifest) {
		t.Fatal("go.mod contains a local replace")
	}
	pseudo := regexp.MustCompile(`-\d{14}-[0-9a-f]+$`)
	for _, line := range strings.Split(manifest, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.HasPrefix(fields[0], "github.com/xbcio/xbc") && (fields[1] == "v0.0.0" || pseudo.MatchString(fields[1])) {
			t.Fatalf("go.mod contains unpublished XBC dependency %q", strings.TrimSpace(line))
		}
	}
}

func TestAutoloadDeclarationExistsOnlyInAutoloadPackage(t *testing.T) {
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
		if strings.Contains(string(data), "autoload.Declare") {
			t.Fatalf("ordinary implementation file %s registers globally", entry.Name())
		}
	}
}
