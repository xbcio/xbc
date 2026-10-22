package automemlimit

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestModuleManifestPinsAutomemlimitWithoutLocalSubstitutions keeps the
// process-wide decision on one reviewed library version and keeps the module
// publishable: no replace, and no unpublished xbc requirement.
func TestModuleManifestPinsAutomemlimitWithoutLocalSubstitutions(t *testing.T) {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	manifest := string(data)

	// go mod tidy renders a lone requirement either as a single-line
	// "require path version" or, inside a block, as the indented bare line.
	pinned := regexp.MustCompile(`(?m)^(?:require\s+)?\s*github\.com/KimMachineGun/automemlimit\s+v1\.0\.0\s*$`)
	if !pinned.MatchString(manifest) {
		t.Fatal("go.mod must pin github.com/KimMachineGun/automemlimit v1.0.0 exactly")
	}
	if regexp.MustCompile(`(?m)^\s*replace\b`).MatchString(manifest) {
		t.Fatal("go.mod must not contain replace directives")
	}
	for _, line := range strings.Split(manifest, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.HasPrefix(fields[0], "github.com/xbcio/xbc") &&
			(fields[1] == "v0.0.0" || regexp.MustCompile(`-\d{14}-[0-9a-f]+$`).MatchString(fields[1])) {
			t.Fatalf("go.mod contains unpublished internal dependency: %s", strings.TrimSpace(line))
		}
	}
}
